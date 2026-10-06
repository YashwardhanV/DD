package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/yashwardhanv/dd/internal/phone"
	"github.com/yashwardhanv/dd/internal/phone/phonetest"
)

// fakePhones makes `adb` report the given device lines, and answer file-list
// requests with listing. It returns a pointer to the device lines so a test can
// "unplug" the phone by changing them.
func fakePhones(t *testing.T, listing string, deviceLines ...string) *[]string {
	t.Helper()
	lines := deviceLines
	adb, _ := phonetest.New(func(_ context.Context, args []string) (string, error) {
		if len(args) > 0 && args[0] == "devices" {
			return phonetest.DevicesOutput(lines...), nil
		}
		return listing, &phone.ExitError{Code: 1} // find exits 1 when some folders are protected
	})
	old := openADB
	openADB = func() (*phone.ADB, error) { return adb, nil }
	t.Cleanup(func() { openADB = old })
	return &lines
}

func sampleListing(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../phone/testdata/adb_find_sample.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestIndexPhoneThenSearchOnlineAndOffline(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", t.TempDir())
	lines := fakePhones(t, sampleListing(t), phonetest.Pixel)

	out, err := run(t, "index", "phone")
	if err != nil {
		t.Fatalf("index phone: %v\n%s", err, out)
	}
	for _, want := range []string{"Indexed phone Pixel 7", "20 entries", "saved (", "Android hides some folders"} {
		if !strings.Contains(out, want) {
			t.Errorf("index output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "Skipped 4 lines") {
		t.Errorf("malformed lines should be reported:\n%s", out)
	}

	// Online: path shown as phone:<model>/..., no "offline".
	out, _ = run(t, "search", "IMG_2024")
	if !strings.Contains(out, "phone:Pixel 7/DCIM/Camera/IMG_20240101_120000.jpg") || strings.Contains(out, "offline") {
		t.Errorf("online phone result:\n%s", out)
	}
	if !strings.Contains(out, "1.0 MB") {
		t.Errorf("size should be shown:\n%s", out)
	}

	// Unplugged: same results, marked offline.
	*lines = nil
	out, _ = run(t, "search", "IMG_2024")
	if !strings.Contains(out, "phone:Pixel 7/DCIM/Camera/IMG_20240101_120000.jpg · offline") {
		t.Errorf("unplugged phone should show · offline:\n%s", out)
	}

	// list shows it with kind and status.
	out, _ = run(t, "list")
	if !strings.Contains(out, "adb-R58M1234ABC") || !strings.Contains(out, "phone") || !strings.Contains(out, "android") || !strings.Contains(out, "offline") {
		t.Errorf("list:\n%s", out)
	}
	*lines = []string{phonetest.Pixel}
	out, _ = run(t, "list")
	if !strings.Contains(out, "online") {
		t.Errorf("list should show the phone online:\n%s", out)
	}

	// Phones and drives are searched together; forget works on phones too.
	if out, err := run(t, "forget", "Pixel 7", "--yes"); err != nil || !strings.Contains(out, "Forgot") {
		t.Errorf("forget phone: %v\n%s", err, out)
	}
}

func TestIndexPhoneByExplicitSerial(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", t.TempDir())
	galaxy := "ZX1G22ABCD             device usb:1-3 product:starlte model:Galaxy_S9 device:starlte transport_id:7"
	fakePhones(t, "5|10|/sdcard/a.txt\n", phonetest.Pixel, galaxy)

	// Two phones and no serial: refuse to guess, list the choices.
	_, err := run(t, "index", "phone")
	var ue *UserError
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "several phones") || !strings.Contains(ue.Hint, "phone:ZX1G22ABCD") || !strings.Contains(ue.Hint, "phone:R58M1234ABC") {
		t.Fatalf("want a 'choose one' error listing both serials, got %v", err)
	}

	out, err := run(t, "index", "phone:zx1g22abcd")
	if err != nil || !strings.Contains(out, "Indexed phone Galaxy S9") {
		t.Fatalf("explicit serial: %v\n%s", err, out)
	}
	out, _ = run(t, "search", "a.txt")
	if !strings.Contains(out, "phone:Galaxy S9/a.txt") {
		t.Errorf("search:\n%s", out)
	}
}

func TestIndexPhoneErrorsAreFriendly(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", t.TempDir())
	cases := []struct {
		name     string
		devices  []string
		arg      string
		wantMsg  string
		wantHint string
	}{
		{"no phone", nil, "phone", "no Android phone is connected", "USB debugging"},
		{"unauthorized", []string{"ABC123  unauthorized usb:1-1 transport_id:2"}, "phone", "is unauthorized", "tap Allow"},
		{"offline", []string{"ABC123  offline transport_id:2"}, "phone", "is offline", "unplug it"},
		{"unknown serial", []string{phonetest.Pixel}, "phone:NOPE", `"NOPE"`, "adb devices"},
	}
	for _, c := range cases {
		fakePhones(t, "", c.devices...)
		_, err := run(t, "index", c.arg)
		var ue *UserError
		if !errors.As(err, &ue) || !strings.Contains(ue.Msg, c.wantMsg) || !strings.Contains(ue.Hint, c.wantHint) {
			t.Errorf("%s: got %v (%+v)", c.name, err, ue)
		}
	}
}

func TestIndexPhoneWithoutAdbExplainsSetup(t *testing.T) {
	old := openADB
	openADB = func() (*phone.ADB, error) { return nil, adbNotFound(phone.ErrADBNotFound) }
	defer func() { openADB = old }()
	_, err := run(t, "index", "phone")
	var ue *UserError
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "adb") || !strings.Contains(ue.Hint, "Build number") || !strings.Contains(ue.Hint, "winget install --id Google.PlatformTools -e") {
		t.Errorf("got %v", err)
	}
	// Multi-line hints are printed aligned (checked via the helper's output shape).
	if !strings.Contains(ue.Hint, "\n") {
		t.Error("setup hint should be multi-line")
	}
}

func TestSearchNeverStartsAdbWithoutPhoneIndexes(t *testing.T) {
	tree, _ := withFixture(t)
	if _, err := run(t, "index", tree); err != nil {
		t.Fatal(err)
	}
	old := openADB
	openADB = func() (*phone.ADB, error) {
		t.Error("adb must not be started when no phone is indexed")
		return nil, errors.New("unexpected")
	}
	defer func() { openADB = old }()
	if _, err := run(t, "search", "resume"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "list"); err != nil {
		t.Fatal(err)
	}
}

func TestPhoneSearchWorksWhenAdbIsMissing(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", t.TempDir())
	fakePhones(t, "5|10|/sdcard/notes.txt\n", phonetest.Pixel)
	if _, err := run(t, "index", "phone"); err != nil {
		t.Fatal(err)
	}
	openADB = func() (*phone.ADB, error) { return nil, adbNotFound(phone.ErrADBNotFound) }
	out, err := run(t, "search", "notes")
	if err != nil || !strings.Contains(out, "phone:Pixel 7/notes.txt · offline") {
		t.Errorf("without adb the phone is just offline, not an error: %v\n%s", err, out)
	}
}

func TestNamesOnlyFallbackIsExplained(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", t.TempDir())
	adb, _ := phonetest.New(func(_ context.Context, args []string) (string, error) {
		switch {
		case args[0] == "devices":
			return phonetest.DevicesOutput(phonetest.Pixel), nil
		case strings.Contains(args[len(args)-1], "stat -c"):
			return "", &phone.ExitError{Code: 1}
		}
		return "/sdcard/DCIM/a.jpg\n", nil
	})
	old := openADB
	openADB = func() (*phone.ADB, error) { return adb, nil }
	defer func() { openADB = old }()

	out, err := run(t, "index", "phone")
	if err != nil || !strings.Contains(out, "only file names were indexed") {
		t.Errorf("err=%v\n%s", err, out)
	}
	out, _ = run(t, "search", "a.jpg")
	if !strings.Contains(out, "a.jpg") || !strings.Contains(out, "?") { // unknown size prints as "?"
		t.Errorf("names-only entries show unknown size:\n%s", out)
	}
}

func TestPhoneScanFailureHasAHint(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", t.TempDir())
	adb, _ := phonetest.New(func(_ context.Context, args []string) (string, error) {
		if args[0] == "devices" {
			return phonetest.DevicesOutput(phonetest.Pixel), nil
		}
		return "", &phone.ExitError{Code: 1, Stderr: "error: closed"}
	})
	old := openADB
	openADB = func() (*phone.ADB, error) { return adb, nil }
	defer func() { openADB = old }()

	_, err := run(t, "index", "phone")
	var ue *UserError
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "couldn't read the files on phone Pixel 7") || !strings.Contains(ue.Hint, "unlock the phone") {
		t.Errorf("got %v", err)
	}
}
