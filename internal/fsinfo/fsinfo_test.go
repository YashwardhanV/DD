package fsinfo

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	sec = int64(time.Second)
	ms  = int64(time.Millisecond)
	hr  = int64(time.Hour)
)

// TestSameTimeTable covers every pair of file systems in the design table
// (NTFS, exFAT, FAT32, phone, portable device), including the one-hour
// daylight-saving shift in both directions.
func TestSameTimeTable(t *testing.T) {
	ntfs, exfat, fat := ForFileSystem("NTFS"), ForFileSystem("exFAT"), ForFileSystem("FAT32")
	phone, device := ForPhone(), ForDevice()
	base := int64(1_700_000_000) * sec

	tests := []struct {
		name       string
		src, dst   Info
		a, b       int64
		coarse     bool
		same, dstS bool
	}{
		{"ntfs-ntfs identical", ntfs, ntfs, base + 123, base + 123, false, true, false},
		{"ntfs-ntfs 100ns apart is the same tick", ntfs, ntfs, base, base + 100, false, true, false},
		{"ntfs-ntfs 1ms apart differs", ntfs, ntfs, base, base + ms, false, false, false},
		{"ntfs-ntfs 1ms apart but coarse source", ntfs, ntfs, base, base + ms, true, true, false},
		{"ntfs-ntfs 2s apart coarse differs", ntfs, ntfs, base, base + 2*sec, true, false, false},
		{"ntfs-exfat 10ms rounding", ntfs, exfat, base + 7*ms, base, false, true, false},
		{"ntfs-exfat 20ms apart differs", ntfs, exfat, base + 20*ms, base, false, false, false},
		{"exfat-ntfs symmetric", exfat, ntfs, base, base + 9*ms, false, true, false},
		{"ntfs-fat32 2s rounding", ntfs, fat, base + 1999*ms, base, false, true, false},
		{"ntfs-fat32 3s differs", ntfs, fat, base + 3*sec, base, false, false, false},
		{"ntfs-fat32 +1h is a DST shift", ntfs, fat, base, base + hr, false, false, true},
		{"ntfs-fat32 -1h is a DST shift", ntfs, fat, base, base - hr, false, false, true},
		{"ntfs-fat32 +1h plus rounding is a DST shift", ntfs, fat, base, base + hr + sec, false, false, true},
		{"ntfs-fat32 +1h and 3s is not a shift", ntfs, fat, base, base + hr + 3*sec, false, false, false},
		{"fat32-ntfs -1h shift (source is FAT)", fat, ntfs, base + hr, base, false, false, true},
		{"exfat-fat32 shift", exfat, fat, base, base - hr, false, false, true},
		{"ntfs-ntfs 1h is just different (UTC both)", ntfs, ntfs, base, base + hr, false, false, false},
		{"exfat-exfat 1h is just different", exfat, exfat, base, base + hr, false, false, false},
		{"phone-ntfs whole seconds", phone, ntfs, base, base + 999*ms, true, true, false},
		{"phone-ntfs 2s apart differs", phone, ntfs, base, base + 2*sec, true, false, false},
		{"phone-fat32 2s rounding", phone, fat, base, base + 2*sec, true, true, false},
		{"phone-fat32 shift", phone, fat, base, base + hr, true, false, true},
		{"device-ntfs unknown resolution is FAT-tolerant", device, ntfs, base, base + 2*sec, true, true, false},
		{"device-ntfs 3s differs", device, ntfs, base, base + 3*sec, true, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			same, shift := SameTime(tc.a, tc.b, tc.src, tc.dst, tc.coarse)
			if same != tc.same || shift != tc.dstS {
				t.Errorf("SameTime = (%v, %v), want (%v, %v)", same, shift, tc.same, tc.dstS)
			}
		})
	}
}

func TestToleranceIsAtLeastASecondForCoarseTimes(t *testing.T) {
	if got := Tolerance(ForFileSystem("NTFS"), ForFileSystem("NTFS"), true); got != time.Second {
		t.Errorf("coarse NTFS tolerance = %v, want 1s", got)
	}
	if got := Tolerance(ForFileSystem("NTFS"), ForFileSystem("NTFS"), false); got != 100*time.Nanosecond {
		t.Errorf("exact NTFS tolerance = %v, want 100ns", got)
	}
}

func TestFileSystemDefaults(t *testing.T) {
	fat := ForFileSystem("FAT32")
	if !fat.IsFAT() || !fat.LocalTime || fat.TimeRes != 2*time.Second {
		t.Errorf("FAT32 = %+v", fat)
	}
	if ForFileSystem("exFAT").IsFAT() {
		t.Error("exFAT is not FAT")
	}
	if !ForFileSystem("NTFS").IsWindowsFS() || ForPhone().IsWindowsFS() || ForDevice().IsWindowsFS() {
		t.Error("IsWindowsFS")
	}
	if !ForPhone().CaseSensitive || ForFileSystem("NTFS").CaseSensitive {
		t.Error("case sensitivity defaults")
	}
}

func TestOfUsesNearestExistingFolderAndCaches(t *testing.T) {
	calls := 0
	restore := SetLookupForTest(func(root string) (Info, error) {
		calls++
		return ForFileSystem("exFAT"), nil
	})
	defer restore()
	dir := t.TempDir()
	missing := filepath.Join(dir, "not", "yet", "created")
	a, err := Of(missing)
	if err != nil || a.FileSystem != "exFAT" {
		t.Fatalf("Of = %+v, %v", a, err)
	}
	if _, err := Of(dir); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("lookup ran %d times, want 1 (cached per volume)", calls)
	}
	if NearestExisting(missing) != dir {
		t.Errorf("NearestExisting = %q", NearestExisting(missing))
	}
	_ = os.Remove(dir)
}
