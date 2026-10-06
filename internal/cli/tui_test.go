package cli

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/tui"
	"github.com/yashwardhanv/dd/internal/volume"
)

func TestTUINeedsATerminal(t *testing.T) {
	// `go test` has no terminal, which is exactly the "piped into a file" situation.
	for _, args := range [][]string{{}, {"tui"}} {
		_, err := run(t, args...)
		var ue *UserError
		if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "needs a terminal") || !strings.Contains(ue.Hint, "drivedex search") {
			t.Errorf("%v: got %v", args, err)
		}
	}
}

func TestPlainDrivedexAndTuiShareTheSameFlags(t *testing.T) {
	out, err := run(t, "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--drive", "--scope", "--query", "tui"} {
		if !strings.Contains(out, want) {
			t.Errorf("root help missing %q", want)
		}
	}
	out, _ = run(t, "tui", "--help")
	for _, want := range []string{"Ctrl+O", "Ctrl+Y", "Ctrl+P", "Enter", "Esc", "type"} {
		if !strings.Contains(out, want) {
			t.Errorf("tui help missing %q", want)
		}
	}
	if _, err := run(t, "bogus"); err == nil {
		t.Error("an unknown command must still be an error, not open the TUI")
	}
}

func TestBuildTUIConfig(t *testing.T) {
	tree, _ := withFixture(t)
	if _, err := run(t, "index", tree); err != nil {
		t.Fatal(err)
	}
	plug(t, fakeDrive("E:"))

	cfg, err := buildTUIConfig(context.Background(), tuiFlags{query: "resume"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Count != 1 || cfg.Query != "resume" || cfg.DriveSourceID != "" || cfg.ScopePath != "" || cfg.IndexScope != nil {
		t.Errorf("config = %+v", cfg)
	}
	if cfg.Actions.Open == nil || cfg.Actions.ShowInFolder == nil || cfg.Actions.CopyText == nil || cfg.Actions.PullPhone == nil {
		t.Error("all four actions must be wired up")
	}
	l, err := cfg.Load()
	if err != nil || len(l.Indexes) != 1 {
		t.Fatalf("Load = %v, %v", l, err)
	}
	if env := cfg.Env(l.Indexes); env.Mounts == nil {
		t.Error("Env must report the connected drives")
	}
}

func TestBuildTUIConfigDriveAndScope(t *testing.T) {
	tree, _ := withFixture(t)
	plug(t, fakeDrive("E:"))
	// The test folder must be indexed as a plain folder. (On Windows the real lookup
	// would find that the folder is on a real drive and index the WHOLE drive.)
	old := volumeForPath
	volumeForPath = func(string) (volume.Volume, error) { return volume.Volume{}, errors.New("not a drive in this test") }
	defer func() { volumeForPath = old }()

	cfg, err := buildTUIConfig(context.Background(), tuiFlags{drive: "e"})
	if err != nil || cfg.DriveSourceID != "vol-1A2B3C4D" {
		t.Errorf("--drive: %+v %v", cfg, err)
	}
	if _, err := buildTUIConfig(context.Background(), tuiFlags{drive: "Q:"}); err == nil || !strings.Contains(err.Error(), "isn't connected") {
		t.Errorf("unknown drive: %v", err)
	}

	cfg, err = buildTUIConfig(context.Background(), tuiFlags{scope: tree})
	if err != nil || cfg.ScopePath != tree || cfg.IndexScope == nil {
		t.Fatalf("--scope: %+v %v", cfg, err)
	}
	if _, err := buildTUIConfig(context.Background(), tuiFlags{scope: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Error("a missing scope folder must be reported")
	}

	// Nothing is indexed yet; the "press I" action indexes the scope.
	store, _ := openStore()
	if infos, _ := store.List(); len(infos) != 0 {
		t.Fatalf("expected no indexes yet, found %d", len(infos))
	}
	if err := cfg.IndexScope(); err != nil {
		t.Fatal(err)
	}
	l, err := store.LoadAll()
	if err != nil || len(l.Indexes) != 1 {
		t.Fatalf("IndexScope should have saved one index: %v %v", l, err)
	}
	// ...and now the folder can be located: the screen would continue to the search.
	loc, ok := index.LocateScope(l.Indexes, index.Env{}, tree)
	if !ok || loc.Index.Header.SourceKind != index.KindFolder {
		t.Errorf("LocateScope = %+v %v", loc, ok)
	}
}

func TestIndexScopeOnADriveIndexesTheWholeDrive(t *testing.T) {
	tree, _ := withFixture(t)
	drive := fakeDrive("E:")
	drive.Root = tree // pretend the drive's root is the fixture folder
	plug(t, drive)
	old := volumeForPath
	volumeForPath = func(string) (volume.Volume, error) { return drive, nil }
	defer func() { volumeForPath = old }()

	if err := indexForScope(context.Background(), filepath.Join(tree, "sub")); err != nil {
		t.Fatal(err)
	}
	store, _ := openStore()
	ix, err := store.Load("vol-1A2B3C4D")
	if err != nil || ix.Header.SourceKind != index.KindVolume {
		t.Fatalf("the whole drive should be indexed as a volume: %v %v", ix, err)
	}
}

func TestIndexingWhileTheScreenIsUpPrintsNothing(t *testing.T) {
	// quiet must not draw a status line; the status line type must tolerate being disabled.
	tree, _ := withFixture(t)
	var out strings.Builder
	if err := indexRoot(context.Background(), &out, tree, index.FolderHeader(tree), scanOptions{quiet: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\r") {
		t.Error("no carriage-return status updates expected")
	}
}

// Compile-time checks that the screen's pieces fit together.
var (
	_ = tui.Run
	_ io.Writer
)
