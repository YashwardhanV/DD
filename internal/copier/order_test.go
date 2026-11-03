package copier

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"
)

func orderPlan() *Plan {
	p := &Plan{}
	add := func(rel string, size int64) {
		p.Items = append(p.Items, Item{Job: Job{Src: "/src/" + rel, Rel: rel, Size: size}})
	}
	add("big1.mkv", 900<<20)
	add("big2.mkv", 500<<20)
	add("c.jpg", 300)
	add("a.jpg", 200)
	add("tiny.txt", 10)
	add("b.jpg", 100)
	return p
}

func rels(p *Plan) string {
	var s []string
	for _, it := range p.Items {
		s = append(s, it.Rel)
	}
	return strings.Join(s, " ")
}

func TestDiskOrderSortsSmallFilesByCluster(t *testing.T) {
	p := orderPlan()
	lcns := map[string]int64{"/src/a.jpg": 900, "/src/b.jpg": 50, "/src/c.jpg": 400}
	lcnOf := func(path string) (int64, error) {
		if path == "/src/tiny.txt" {
			return 0, ErrResident // inside the MFT: first
		}
		return lcns[path], nil
	}
	used, why := p.OrderSmallFiles(context.Background(), OrderDisk, lcnOf, time.Second)
	if used != OrderDisk || why != "" {
		t.Fatalf("used %q why %q", used, why)
	}
	if got := rels(p); got != "big1.mkv big2.mkv tiny.txt b.jpg c.jpg a.jpg" {
		t.Errorf("order: %s", got)
	}
}

func TestDiskOrderFallsBack(t *testing.T) {
	p := orderPlan()
	before := rels(p)
	used, why := p.OrderSmallFiles(context.Background(), OrderDisk, func(string) (int64, error) { return 0, errors.ErrUnsupported }, time.Second)
	if used != OrderSize || !strings.Contains(why, "not NTFS") || rels(p) != before {
		t.Errorf("non-NTFS: %q %q %s", used, why, rels(p))
	}
	// Too slow: the 10% budget runs out.
	slow := func(string) (int64, error) { time.Sleep(20 * time.Millisecond); return 1, nil }
	used, why = p.OrderSmallFiles(context.Background(), OrderDisk, slow, 5*time.Millisecond)
	if used != OrderSize || !strings.Contains(why, "longer than 10%") || rels(p) != before {
		t.Errorf("slow: %q %q %s", used, why, rels(p))
	}
}

func TestNameAndSizeOrder(t *testing.T) {
	p := orderPlan()
	if used, _ := p.OrderSmallFiles(context.Background(), OrderName, nil, 0); used != OrderName || rels(p) != "big1.mkv big2.mkv a.jpg b.jpg c.jpg tiny.txt" {
		t.Errorf("name: %s", rels(p))
	}
	p = orderPlan()
	before := rels(p)
	if used, _ := p.OrderSmallFiles(context.Background(), OrderSize, nil, 0); used != OrderSize || rels(p) != before {
		t.Errorf("size: %s", rels(p))
	}
}

func TestDiskOrderBudget(t *testing.T) {
	// 10,000 files, 1 GB: about 12.5 s + 80 s = 92.5 s -> 9.25 s.
	if b := DiskOrderBudget(10000, 1e9); b < 9*time.Second || b > 10*time.Second {
		t.Errorf("budget %v", b)
	}
}

func TestParseRetrievalPointers(t *testing.T) {
	buf := make([]byte, 32)
	if _, err := ParseRetrievalPointers(buf); !errors.Is(err, ErrResident) {
		t.Errorf("no extents: %v", err)
	}
	binary.LittleEndian.PutUint32(buf, 1)
	binary.LittleEndian.PutUint64(buf[24:], 123456)
	if lcn, err := ParseRetrievalPointers(buf); err != nil || lcn != 123456 {
		t.Errorf("%d %v", lcn, err)
	}
	if _, err := ParseRetrievalPointers(buf[:10]); err == nil {
		t.Error("short buffer")
	}
}
