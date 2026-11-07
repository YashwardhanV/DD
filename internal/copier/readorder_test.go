package copier

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDiskOrderReadsSmallFilesByDiskPosition(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	pos := map[string]int64{"c.txt": 10, "a.txt": 30, "b.txt": 20}
	for name := range pos {
		writeAt(t, filepath.Join(src, name), "x", time.Unix(1_700_000_000, 0))
	}
	writeAt(t, filepath.Join(src, "resident.txt"), "y", time.Unix(1_700_000_000, 0)) // no cluster
	old := firstLCN
	firstLCN = func(p string) (int64, bool) { v, ok := pos[filepath.Base(p)]; return v, ok }
	defer func() { firstLCN = old }()

	var mu sync.Mutex
	var order []string
	openSource = func(name string, locked bool) (*os.File, error) {
		mu.Lock()
		order = append(order, filepath.Base(name))
		mu.Unlock()
		return defaultOpenSource(name, locked)
	}
	defer func() { openSource = defaultOpenSource }()

	opt := fastOpts(1)
	opt.ReadOrder = ReadOrderDisk
	res := mustRun(t, plan(t, src, dst, ""), opt)
	if res.Written != 4 {
		t.Fatalf("result %+v", res)
	}
	if got := strings.Join(order, ","); got != "c.txt,b.txt,a.txt,resident.txt" {
		t.Errorf("read order %s, want disk order then the file without a cluster", got)
	}
}

func TestValidReadOrder(t *testing.T) {
	for _, s := range []string{"", "auto", "size", "disk"} {
		if !ValidReadOrder(s) {
			t.Errorf("%q rejected", s)
		}
	}
	if ValidReadOrder("random") {
		t.Error("random accepted")
	}
}
