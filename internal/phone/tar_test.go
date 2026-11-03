package phone_test

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/yashwardhanv/dd/internal/phone"
	"github.com/yashwardhanv/dd/internal/phone/phonetest"
)

func TestParseCminProbe(t *testing.T) {
	cases := map[string]bool{
		"DDX_RC=0\n":    true,
		"/\nDDX_RC=0\n": true,
		"find: Unknown option '-cmin'\nDDX_RC=1\n": false,
		"find: bad arg '-cmin'\nDDX_RC=0\n":        false,
		"DDX_RC=1\n":                               false,
		"":                                         false,
	}
	for out, want := range cases {
		if got := phone.ParseCminProbe(out); got != want {
			t.Errorf("ParseCminProbe(%q) = %v", out, got)
		}
	}
}

func tarOf(entries ...tar.Header) []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, h := range entries {
		h := h
		tw.WriteHeader(&h)
		tw.Write(make([]byte, h.Size))
	}
	tw.Close()
	return b.Bytes()
}

func TestParseTarProbe(t *testing.T) {
	if !phone.ParseTarProbe(tarOf(tar.Header{Name: "hosts", Size: 3, Typeflag: tar.TypeReg})) {
		t.Error("a one-file archive named hosts is a working tar")
	}
	if !phone.ParseTarProbe(tarOf(tar.Header{Name: "./hosts", Size: 3, Typeflag: tar.TypeReg})) {
		t.Error("./hosts is the same file")
	}
	for _, bad := range [][]byte{
		nil, []byte("tar: unknown option"),
		tarOf(tar.Header{Name: "hosts", Typeflag: tar.TypeSymlink, Linkname: "x"}),
		tarOf(tar.Header{Name: "hosts", Size: 1, Typeflag: tar.TypeReg}, tar.Header{Name: "b", Size: 1, Typeflag: tar.TypeReg}),
		tarOf(tar.Header{Name: "other", Size: 1, Typeflag: tar.TypeReg}),
	} {
		if phone.ParseTarProbe(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestTarNames(t *testing.T) {
	cases := []struct {
		in, clean string
		safe      bool
	}{
		{"DCIM/a.jpg", "DCIM/a.jpg", true},
		{"./DCIM/a.jpg", "DCIM/a.jpg", true},
		{"DCIM//x/../a.jpg", "DCIM/a.jpg", true},
		{"../evil", "../evil", false},
		{"a/../../evil", "../evil", false},
		{"/etc/passwd", "/etc/passwd", false},
		{".", ".", false},
		{"", "", false},
	}
	for _, c := range cases {
		got := phone.CleanTarName(c.in)
		if got != c.clean || phone.SafeTarName(got) != c.safe {
			t.Errorf("%q -> %q safe=%v", c.in, got, phone.SafeTarName(got))
		}
	}
}

func TestTarCommandQuoting(t *testing.T) {
	cmd := phone.TarCommand("/sdcard/DCIM", []string{"Camera/a.jpg", "it's here.jpg", "-dash.jpg", "sp ace/x y.png"})
	want := `cd /sdcard/DCIM && tar -cf - -- Camera/a.jpg 'it'\''s here.jpg' -dash.jpg 'sp ace/x y.png' 2>/dev/null`
	if cmd != want {
		t.Errorf("got  %s\nwant %s", cmd, want)
	}
}

func TestParsePullCompress(t *testing.T) {
	help := "pull [-a] [-z ALGORITHM] [-Z] REMOTE... LOCAL"
	if got := phone.ParsePullCompress(help, "shell_v2,sendrecv_v2,sendrecv_v2_zstd,sendrecv_v2_lz4\n"); strings.Join(got, ",") != "lz4,zstd" {
		t.Errorf("got %v", got)
	}
	if got := phone.ParsePullCompress("pull [-a] REMOTE LOCAL", "sendrecv_v2_lz4"); got != nil {
		t.Errorf("an old adb without -z: %v", got)
	}
	if got := phone.ParsePullCompress(help, "shell_v2,cmd"); got != nil {
		t.Errorf("an old phone: %v", got)
	}
}

func TestProbeAndTarStreamWithTheFakePhone(t *testing.T) {
	fp := phonetest.NewFS()
	fp.Features = "sendrecv_v2,sendrecv_v2_brotli,sendrecv_v2_lz4"
	fp.Add("/sdcard/DCIM/a.jpg", "AAAA", 1_700_000_000)
	fp.Add("/sdcard/DCIM/it's.jpg", "BB", 1_700_000_001)
	adb := fp.ADB()
	c, err := adb.Probe(context.Background(), "R58M1234ABC")
	if err != nil || !c.Tar || !c.FindCmin || strings.Join(c.PullCompress, ",") != "lz4,brotli" {
		t.Fatalf("caps %+v %v", c, err)
	}
	stream, wait, err := adb.TarStream(context.Background(), "R58M1234ABC", "/sdcard/DCIM", []string{"a.jpg", "it's.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(stream)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		names = append(names, h.Name+"="+string(data))
	}
	if err := wait(); err != nil {
		t.Error(err)
	}
	if strings.Join(names, " ") != "a.jpg=AAAA it's.jpg=BB" {
		t.Errorf("%q", names)
	}

	fp2 := phonetest.NewFS()
	fp2.NoTar = true
	if c, _ := fp2.ADB().Probe(context.Background(), "X"); c.Tar || c.PullCompress != nil {
		t.Errorf("no tar, no features: %+v", c)
	}
}
