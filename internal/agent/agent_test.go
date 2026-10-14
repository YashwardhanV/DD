package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/phone"
	"github.com/yashwardhanv/dd/internal/portable"
	"github.com/yashwardhanv/dd/internal/volume"
)

// world is a fake PC: which drives and phones are plugged in, what has been
// indexed, which pop-ups were shown.
type world struct {
	mu       sync.Mutex
	vols     []volume.Volume
	mask     uint32
	bus      map[string]volume.BusInfo // by drive root
	busErr   map[string]error
	devs     []phone.Device
	devsErr  error
	ports    []portable.Device
	portsErr error
	headers  map[string]index.Header
	locked   map[string]bool
	indexErr map[string]error
	entries  int

	indexed   []Target
	notified  [][2]string
	volCalls  atomic.Int64
	sleeps    []time.Duration
	notifyErr error
	running   atomic.Int64
	peak      atomic.Int64
	indexWork time.Duration
	now       time.Time
	links     map[string]volume.USBLink // by drive root; missing = not USB

	logPath string
}

func newWorld(t *testing.T) *world {
	return &world{
		bus: map[string]volume.BusInfo{}, busErr: map[string]error{}, headers: map[string]index.Header{},
		locked: map[string]bool{}, indexErr: map[string]error{}, entries: 182334,
		now: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC), logPath: filepath.Join(t.TempDir(), "agent.log"),
	}
}

func usbDrive(letter, label string, serial uint32) volume.Volume {
	return volume.Volume{Root: letter + `\`, Label: label, FileSystem: "exFAT", Serial: serial, DriveType: volume.DriveRemovable, Ready: true}
}

func (w *world) plug(v volume.Volume, bus volume.BusInfo) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.vols = append(w.vols, v)
	w.bus[v.Root] = bus
	w.mask |= 1 << (v.Root[0] - 'A')
}

func (w *world) unplug(letter string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var keep []volume.Volume
	for _, v := range w.vols {
		if !strings.HasPrefix(v.Root, letter) {
			keep = append(keep, v)
		}
	}
	w.vols = keep
	w.mask &^= 1 << (letter[0] - 'A')
}

func (w *world) deps() Deps {
	return Deps{
		Mask: func() (uint32, error) { w.mu.Lock(); defer w.mu.Unlock(); return w.mask, nil },
		Volumes: func() ([]volume.Volume, error) {
			w.volCalls.Add(1)
			w.mu.Lock()
			defer w.mu.Unlock()
			return append([]volume.Volume(nil), w.vols...), nil
		},
		Bus: func(root string) (volume.BusInfo, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			return w.bus[root], w.busErr[root]
		},
		Devices: func(context.Context) ([]phone.Device, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			return append([]phone.Device(nil), w.devs...), w.devsErr
		},
		Portables: func() ([]portable.Device, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			return append([]portable.Device(nil), w.ports...), w.portsErr
		},
		SystemDrive: "C:",
		Index: func(ctx context.Context, t Target) (Outcome, error) {
			n := w.running.Add(1)
			defer w.running.Add(-1)
			for {
				p := w.peak.Load()
				if n <= p || w.peak.CompareAndSwap(p, n) {
					break
				}
			}
			if w.indexWork > 0 {
				select {
				case <-time.After(w.indexWork):
				case <-ctx.Done():
					return Outcome{}, ctx.Err()
				}
			}
			w.mu.Lock()
			defer w.mu.Unlock()
			w.indexed = append(w.indexed, t)
			if err := w.indexErr[t.ID]; err != nil {
				return Outcome{}, err
			}
			return Outcome{Entries: w.entries}, nil
		},
		Locked: func(t Target) bool { w.mu.Lock(); defer w.mu.Unlock(); return w.locked[t.ID] },
		Header: func(id string) (index.Header, bool) {
			w.mu.Lock()
			defer w.mu.Unlock()
			h, ok := w.headers[id]
			return h, ok
		},
		Notify: func(title, body string) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.notified = append(w.notified, [2]string{title, body})
			return w.notifyErr
		},
		Sleep: func(ctx context.Context, d time.Duration) bool {
			w.mu.Lock()
			w.sleeps = append(w.sleeps, d)
			w.mu.Unlock()
			return ctx.Err() == nil
		},
		Now: func() time.Time { return w.now },
		USBLink: func(root string) (volume.USBLink, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			if l, ok := w.links[root]; ok {
				return l, nil
			}
			return volume.USBLink{}, volume.ErrNotUSB
		},
	}
}

func (w *world) agent(t *testing.T, mutate ...func(*Settings)) (*Agent, *Logger) {
	t.Helper()
	log, err := NewLogger(w.logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	set := DefaultSettings()
	for _, m := range mutate {
		m(&set)
	}
	return New(w.deps(), set, log), log
}

func (w *world) logText(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(w.logPath)
	return string(b)
}

var usb = volume.BusInfo{Bus: volume.BusTypeUsb, Vendor: "SanDisk", Product: "Ultra"}

// ---- volumes -------------------------------------------------------------------------

func TestPresentDrivesAreNotArrivals(t *testing.T) {
	w := newWorld(t)
	w.plug(usbDrive("E:", "SANDISK 32GB", 0x1111), usb)
	a, _ := w.agent(t)
	a.Prime(context.Background())
	if got := a.PollVolumes(context.Background()); len(got) != 0 {
		t.Errorf("a drive that was already plugged in at start-up must not count as arriving: %+v", got)
	}
}

func TestNewUSBDriveIsIndexedWithItsLabelAndLetter(t *testing.T) {
	w := newWorld(t)
	a, _ := w.agent(t)
	a.Prime(context.Background())

	w.plug(usbDrive("F:", "SANDISK 32GB", 0x1A2B3C4D), usb)
	got := a.PollVolumes(context.Background())
	if len(got) != 1 {
		t.Fatalf("arrivals = %+v", got)
	}
	want := Target{Kind: index.KindVolume, ID: "vol-1A2B3C4D", Label: "SANDISK 32GB", Arg: "F:", Display: "SANDISK 32GB (F:)"}
	if got[0] != want {
		t.Errorf("target = %+v, want %+v", got[0], want)
	}
	// Seen once: not reported again while it stays plugged in.
	if again := a.PollVolumes(context.Background()); len(again) != 0 {
		t.Errorf("repeated arrival: %+v", again)
	}
}

func TestOnlyExternalDrivesAreAutoIndexedUnlessListed(t *testing.T) {
	w := newWorld(t)
	a, _ := w.agent(t, func(s *Settings) { s.AutoIndex = []string{"d", `G:\`} })
	a.Prime(context.Background())

	w.plug(volume.Volume{Root: `C:\`, Label: "Windows", Serial: 0xC0, DriveType: volume.DriveFixed, Ready: true}, volume.BusInfo{Bus: volume.BusTypeNvme})
	w.plug(volume.Volume{Root: `D:\`, Label: "Data", Serial: 0xD0, DriveType: volume.DriveFixed, Ready: true}, volume.BusInfo{Bus: volume.BusTypeSata})
	w.plug(volume.Volume{Root: `H:\`, Label: "Other", Serial: 0xE0, DriveType: volume.DriveFixed, Ready: true}, volume.BusInfo{Bus: volume.BusTypeSata})
	w.plug(usbDrive("G:", "GAMES", 0x60), volume.BusInfo{Bus: volume.BusTypeSata}) // not USB, but listed
	w.plug(usbDrive("E:", "PEN", 0xE1), usb)

	var ids []string
	for _, tg := range a.PollVolumes(context.Background()) {
		ids = append(ids, tg.Arg)
	}
	// Targets come out sorted by ID (vol-00000060 G:, vol-000000D0 D:, vol-000000E1 E:).
	if strings.Join(ids, ",") != "G:,D:,E:" {
		t.Errorf("auto-indexed = %v, want G: D: E: (C: and H: are internal SATA/NVMe drives that aren't listed)", ids)
	}
	if !strings.Contains(w.logText(t), "ignoring C: (Windows): internal drive (NVMe)") {
		t.Errorf("ignored drives should be logged:\n%s", w.logText(t))
	}
}

func TestReplugCountsAsANewArrivalEvenAtAnotherLetter(t *testing.T) {
	w := newWorld(t)
	a, _ := w.agent(t)
	a.Prime(context.Background())

	w.plug(usbDrive("E:", "PEN", 0x77), usb)
	if got := a.PollVolumes(context.Background()); len(got) != 1 || got[0].Arg != "E:" {
		t.Fatalf("first plug: %+v", got)
	}
	w.unplug("E:")
	if got := a.PollVolumes(context.Background()); len(got) != 0 {
		t.Fatalf("unplugging is not an arrival: %+v", got)
	}
	w.plug(usbDrive("G:", "PEN", 0x77), usb) // same drive, Windows gave it G: this time
	got := a.PollVolumes(context.Background())
	if len(got) != 1 || got[0].ID != "vol-00000077" || got[0].Arg != "G:" {
		t.Errorf("replug at G: should arrive again, found by serial: %+v", got)
	}
}

func TestUnusableVolumesAreIgnored(t *testing.T) {
	w := newWorld(t)
	a, _ := w.agent(t)
	a.Prime(context.Background())
	w.plug(volume.Volume{Root: `H:\`, DriveType: volume.DriveRemovable}, usb)                                    // empty card reader
	w.plug(volume.Volume{Root: `I:\`, Serial: 0, DriveType: volume.DriveRemovable, Ready: true}, usb)            // no serial
	w.plug(volume.Volume{Root: `Z:\`, Serial: 9, DriveType: volume.DriveRemote, Ready: true, Label: "NAS"}, usb) // network
	if got := a.PollVolumes(context.Background()); len(got) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestInsertingACardIsNoticedEvenThoughTheLettersDontChange(t *testing.T) {
	w := newWorld(t)
	w.plug(volume.Volume{Root: `H:\`, DriveType: volume.DriveRemovable}, usb) // card reader, empty
	a, _ := w.agent(t, func(s *Settings) { s.DrivePoll, s.MediaPoll = 2*time.Second, 10*time.Second })
	a.Prime(context.Background())
	base := w.volCalls.Load()

	// Four quiet polls: the cheap letter check says "nothing changed" - drives aren't even listed.
	for i := 0; i < 4; i++ {
		a.PollVolumes(context.Background())
	}
	if w.volCalls.Load() != base {
		t.Errorf("the drive list was read %d times during quiet polls", w.volCalls.Load()-base)
	}

	// A card is inserted: same letter, but now it's ready.
	w.mu.Lock()
	w.vols[0] = usbDrive("H:", "SD CARD", 0x5D)
	w.mu.Unlock()
	got := a.PollVolumes(context.Background()) // the 5th poll = 10 s: periodic full check
	if len(got) != 1 || got[0].Arg != "H:" || got[0].Label != "SD CARD" {
		t.Errorf("card insertion not detected: %+v", got)
	}
}

func TestLabelFallbacks(t *testing.T) {
	w := newWorld(t)
	a, _ := w.agent(t)
	a.Prime(context.Background())
	w.plug(usbDrive("E:", "", 0xE1), volume.BusInfo{Bus: volume.BusTypeUsb, Vendor: "Kingston", Product: "DataTraveler"})
	w.plug(usbDrive("F:", "  ", 0xF1), volume.BusInfo{Bus: volume.BusTypeUsb})
	byLetter := map[string]string{}
	for _, tg := range a.PollVolumes(context.Background()) {
		byLetter[tg.Arg] = tg.Label
	}
	if byLetter["E:"] != "Kingston DataTraveler" || byLetter["F:"] != "Drive F:" {
		t.Errorf("labels = %v", byLetter)
	}
}

func TestBusQueryFailureJudgesByDriveTypeAndIsLoggedOnce(t *testing.T) {
	w := newWorld(t)
	a, _ := w.agent(t)
	a.Prime(context.Background())
	disk := volume.Volume{Root: `E:\`, Label: "DISK", Serial: 0x1, DriveType: volume.DriveFixed, Ready: true}
	w.plug(disk, usb)
	w.busErr[`E:\`] = errors.New("access denied")
	for i := 0; i < 3; i++ {
		if got := a.PollVolumes(context.Background()); len(got) != 0 {
			t.Errorf("a fixed drive whose bus is unknown must not be auto-indexed: %+v", got)
		}
		w.unplug("E:")
		w.plug(disk, usb)
	}
	// A "removable" drive (pen drive, memory card) is external whatever its bus.
	w.plug(usbDrive("F:", "PEN", 0x2), usb)
	w.busErr[`F:\`] = errors.New("access denied")
	if got := a.PollVolumes(context.Background()); len(got) != 1 || got[0].Arg != "F:" {
		t.Errorf("a removable drive must be auto-indexed even if its bus is unknown: %+v", got)
	}
	if n := strings.Count(w.logText(t), "couldn't tell how E: is connected"); n != 1 {
		t.Errorf("the problem should be logged once, not %d times", n)
	}
}

func TestEveryKindOfExternalDiskIsAutoIndexed(t *testing.T) {
	w := newWorld(t)
	a, _ := w.agent(t)
	a.Prime(context.Background())
	fixed := func(letter, label string, serial uint32) volume.Volume {
		return volume.Volume{Root: letter + `\`, Label: label, Serial: serial, DriveType: volume.DriveFixed, Ready: true}
	}
	w.plug(fixed("E:", "USB HDD", 0xE0), volume.BusInfo{Bus: volume.BusTypeUsb})
	w.plug(fixed("F:", "T7 THUNDERBOLT", 0xF0), volume.BusInfo{Bus: volume.BusTypeNvme, Hotplug: true})
	w.plug(fixed("G:", "ESATA", 0x60), volume.BusInfo{Bus: volume.BusTypeSata, Hotplug: true})
	w.plug(fixed("H:", "SD CARD", 0x70), volume.BusInfo{Bus: volume.BusTypeSd}) // laptop's built-in card reader
	w.plug(fixed("D:", "DATA", 0xD0), volume.BusInfo{Bus: volume.BusTypeSata})  // internal: left alone
	w.plug(fixed("V:", "VHD", 0x50), volume.BusInfo{Bus: volume.BusTypeFileBackedVirtual, Hotplug: true})

	var args []string
	for _, tg := range a.PollVolumes(context.Background()) {
		args = append(args, tg.Arg)
	}
	if strings.Join(args, ",") != "G:,H:,E:,F:" {
		t.Errorf("auto-indexed = %v, want the USB, Thunderbolt, eSATA and SD drives (not the internal disk or the .vhdx)", args)
	}
}

func TestTheWindowsDriveIsNeverAutoIndexed(t *testing.T) {
	w := newWorld(t)
	w.plug(volume.Volume{Root: `C:\`, Label: "Windows To Go", Serial: 0xC0, DriveType: volume.DriveFixed, Ready: true}, usb)
	a, _ := w.agent(t)
	if got := a.Prime(context.Background()); len(got) != 0 {
		t.Errorf("Windows running from a USB disk must not index its own system drive: %+v", got)
	}
}

// ---- start-up catch-up -----------------------------------------------------------------

func TestPrimeRefreshesMissingAndStaleIndexesOnly(t *testing.T) {
	w := newWorld(t)
	w.plug(usbDrive("E:", "FRESH", 0xE1), usb)
	w.plug(usbDrive("F:", "STALE", 0xF1), usb)
	w.plug(usbDrive("G:", "NEVER", 0x61), usb)
	w.plug(volume.Volume{Root: `D:\`, Label: "Internal", Serial: 0xD1, DriveType: volume.DriveFixed, Ready: true}, volume.BusInfo{Bus: volume.BusTypeSata})
	w.headers["vol-000000E1"] = index.Header{UpdatedAt: w.now.Add(-2 * time.Hour)}
	w.headers["vol-000000F1"] = index.Header{UpdatedAt: w.now.Add(-30 * time.Hour)}

	a, _ := w.agent(t)
	var args []string
	for _, tg := range a.Prime(context.Background()) {
		args = append(args, tg.Arg)
	}
	// Sorted by ID: vol-00000061 (G:, never indexed) then vol-000000F1 (F:, stale). The fresh E: is left alone.
	if strings.Join(args, ",") != "G:,F:" {
		t.Errorf("catch-up = %v, want the never-indexed (G:) and stale (F:) USB drives only", args)
	}
}

func TestPrimeCatchUpForPhones(t *testing.T) {
	w := newWorld(t)
	w.devs = []phone.Device{
		{Serial: "AAA", State: "device", Model: "Pixel 7"},
		{Serial: "BBB", State: "unauthorized"},
		{Serial: "CCC", State: "device", Model: "Galaxy"},
	}
	w.headers["adb-CCC"] = index.Header{UpdatedAt: w.now.Add(-time.Hour)}
	a, _ := w.agent(t)
	got := a.Prime(context.Background())
	if len(got) != 1 || got[0].Arg != "phone:AAA" || got[0].Label != "Pixel 7" {
		t.Errorf("catch-up = %+v", got)
	}
}

// ---- phones -------------------------------------------------------------------------------

func TestPhoneArrivalAndAuthorization(t *testing.T) {
	w := newWorld(t)
	a, _ := w.agent(t)
	a.Prime(context.Background())

	w.devs = []phone.Device{{Serial: "R58", State: "unauthorized"}}
	if got := a.PollPhones(context.Background()); len(got) != 0 {
		t.Errorf("an unauthorized phone can't be indexed yet: %+v", got)
	}
	a.PollPhones(context.Background())
	if n := strings.Count(w.logText(t), "waiting for you to tap Allow"); n != 1 {
		t.Errorf("the authorization hint should be logged once, got %d", n)
	}

	w.devs = []phone.Device{{Serial: "R58", State: "device", Model: "Pixel 7"}} // the user tapped Allow
	got := a.PollPhones(context.Background())
	want := Target{Kind: index.KindPhone, ID: "adb-R58", Label: "Pixel 7", Arg: "phone:R58", Display: "phone Pixel 7 (R58)"}
	if len(got) != 1 || got[0] != want {
		t.Errorf("arrival = %+v", got)
	}
	if again := a.PollPhones(context.Background()); len(again) != 0 {
		t.Error("a connected phone must not arrive repeatedly")
	}
	w.devs = nil
	a.PollPhones(context.Background())
	w.devs = []phone.Device{{Serial: "R58", State: "device", Model: "Pixel 7"}}
	if again := a.PollPhones(context.Background()); len(again) != 1 {
		t.Error("replugging a phone is a new arrival")
	}
}

func TestAdbProblemsAreLoggedOnceAndDontStopTheAgent(t *testing.T) {
	w := newWorld(t)
	a, _ := w.agent(t)
	w.devsErr = errors.New("adb server didn't start")
	for i := 0; i < 4; i++ {
		if got := a.PollPhones(context.Background()); got != nil {
			t.Errorf("got %+v", got)
		}
	}
	if n := strings.Count(w.logText(t), "adb server didn't start"); n != 1 {
		t.Errorf("logged %d times", n)
	}
	w.devsErr = nil
	w.devs = []phone.Device{{Serial: "X", State: "device"}}
	if got := a.PollPhones(context.Background()); len(got) != 1 {
		t.Errorf("recovery: %+v", got)
	}

	// Without adb at all, phones are simply not watched.
	d := w.deps()
	d.Devices = nil
	if got := New(d, DefaultSettings(), nil).PollPhones(context.Background()); got != nil {
		t.Errorf("got %+v", got)
	}
}

// ---- portable devices (MTP/PTP) -----------------------------------------------------------

var galaxy = portable.Device{
	InterfacePath: `\\?\usb#vid_04e8&pid_6860&ms_comp_mtp&samsung_android#6&2b5c4a3c&0&0000#{6ac27878-a6fa-4155-ba85-f98f491d4f33}`,
	InstanceID:    `USB\VID_04E8&PID_6860&MS_COMP_MTP&SAMSUNG_ANDROID\6&2B5C4A3C&0&0000`,
	Serial:        "R58M12345", Name: "Galaxy S21",
}

var camera = portable.Device{InstanceID: `USB\VID_04A9&PID_32D9\6&1&0&0`, Name: "Canon EOS R50"}

func TestPortableDeviceArrival(t *testing.T) {
	w := newWorld(t)
	a, _ := w.agent(t)
	a.Prime(context.Background())

	w.ports = []portable.Device{galaxy}
	got := a.PollPortables(context.Background())
	want := Target{Kind: index.KindDevice, ID: galaxy.SourceID(), Label: "Galaxy S21", Arg: "device:" + galaxy.SourceID(), Display: "device Galaxy S21"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("arrival = %+v, want %+v", got, want)
	}
	if again := a.PollPortables(context.Background()); len(again) != 0 {
		t.Error("a connected device must not arrive repeatedly")
	}
	w.ports = append(w.ports, camera)
	if got := a.PollPortables(context.Background()); len(got) != 1 || got[0].Label != "Canon EOS R50" {
		t.Errorf("second device = %+v", got)
	}
	w.ports = nil
	a.PollPortables(context.Background())
	w.ports = []portable.Device{galaxy}
	if got := a.PollPortables(context.Background()); len(got) != 1 {
		t.Error("replugging a device is a new arrival")
	}
}

func TestAPhoneThatAdbCanReadIsNotReadTwice(t *testing.T) {
	w := newWorld(t)
	a, _ := w.agent(t)
	a.Prime(context.Background())

	// USB debugging is on: adb reports the phone, and the same phone shows up as a portable device.
	w.devs = []phone.Device{{Serial: "r58m12345", State: "device", Model: "SM-G991B"}}
	w.ports = []portable.Device{galaxy}
	if got := a.PollPhones(context.Background()); len(got) != 1 || got[0].Kind != index.KindPhone {
		t.Fatalf("adb arrival = %+v", got)
	}
	if got := a.PollPortables(context.Background()); len(got) != 0 {
		t.Errorf("the phone is already indexed over adb (faster): %+v", got)
	}
	if !strings.Contains(w.logText(t), "Galaxy S21 is connected with USB debugging") {
		t.Errorf("the choice should be logged:\n%s", w.logText(t))
	}
	// An unauthorized adb phone can't be read by adb: the portable device is used.
	w2 := newWorld(t)
	b, _ := w2.agent(t)
	b.Prime(context.Background())
	w2.devs = []phone.Device{{Serial: "R58M12345", State: "unauthorized"}}
	w2.ports = []portable.Device{galaxy}
	b.PollPhones(context.Background())
	if got := b.PollPortables(context.Background()); len(got) != 1 {
		t.Errorf("unauthorized adb: the portable device should be indexed: %+v", got)
	}
}

func TestALockedPhoneIsRetriedWithBackoff(t *testing.T) {
	w := newWorld(t)
	w.indexErr[galaxy.SourceID()] = errors.New("the device isn't showing any storage")
	a, _ := w.agent(t)
	a.Prime(context.Background())
	w.ports = []portable.Device{galaxy}

	got := a.PollPortables(context.Background())
	if len(got) != 1 {
		t.Fatalf("arrival = %+v", got)
	}
	a.Handle(context.Background(), got[0]) // fails: the phone is locked
	if !strings.Contains(w.logText(t), "will try device Galaxy S21 again in 30.0s") {
		t.Errorf("the retry should be logged:\n%s", w.logText(t))
	}
	if got := a.PollPortables(context.Background()); len(got) != 0 {
		t.Error("not retried before 30 s")
	}
	w.now = w.now.Add(30 * time.Second)
	got = a.PollPortables(context.Background())
	if len(got) != 1 {
		t.Fatal("retried after 30 s")
	}
	if again := a.PollPortables(context.Background()); len(again) != 0 {
		t.Error("a queued retry must not be queued again")
	}
	a.Handle(context.Background(), got[0]) // still locked: wait twice as long
	w.now = w.now.Add(59 * time.Second)
	if got := a.PollPortables(context.Background()); len(got) != 0 {
		t.Error("the second retry waits 60 s")
	}
	w.now = w.now.Add(time.Second)
	got = a.PollPortables(context.Background())
	if len(got) != 1 {
		t.Fatal("retried after 60 s")
	}
	// The user unlocks the phone: it is indexed, and not retried again.
	delete(w.indexErr, galaxy.SourceID())
	a.Handle(context.Background(), got[0])
	w.now = w.now.Add(time.Hour)
	if got := a.PollPortables(context.Background()); len(got) != 0 {
		t.Errorf("an indexed device must not be retried: %+v", got)
	}
	if len(w.notified) != 1 || w.notified[0][0] != "Galaxy S21 indexed" {
		t.Errorf("notified = %v", w.notified)
	}
}

func TestRetryDelayIsCapped(t *testing.T) {
	w := newWorld(t)
	a, _ := w.agent(t)
	tg := deviceTarget(camera)
	for i := 0; i < 10; i++ {
		a.deviceFailed(tg)
	}
	if d := a.retries[tg.ID].delay; d != 5*time.Minute {
		t.Errorf("delay = %v, want the 5 min cap", d)
	}
	// Unplugging forgets the backoff.
	w.ports = nil
	a.PollPortables(context.Background())
	if _, ok := a.retries[tg.ID]; ok {
		t.Error("an unplugged device's retry must be dropped")
	}
}

func TestPrimeCatchUpForPortableDevices(t *testing.T) {
	w := newWorld(t)
	w.ports = []portable.Device{galaxy, camera}
	w.headers[camera.SourceID()] = index.Header{UpdatedAt: w.now.Add(-time.Hour)}
	a, _ := w.agent(t)
	got := a.Prime(context.Background())
	if len(got) != 1 || got[0].ID != galaxy.SourceID() {
		t.Errorf("catch-up = %+v (the camera was indexed an hour ago)", got)
	}
	if again := a.PollPortables(context.Background()); len(again) != 0 {
		t.Errorf("devices present at start-up are not arrivals: %+v", again)
	}
}

func TestPortableListingProblemsAreLoggedOnce(t *testing.T) {
	w := newWorld(t)
	a, _ := w.agent(t)
	w.portsErr = errors.New("cfgmgr32 said no")
	for i := 0; i < 3; i++ {
		if got := a.PollPortables(context.Background()); got != nil {
			t.Errorf("got %+v", got)
		}
	}
	if n := strings.Count(w.logText(t), "cfgmgr32 said no"); n != 1 {
		t.Errorf("logged %d times", n)
	}
	d := w.deps()
	d.Portables = nil
	if got := New(d, DefaultSettings(), nil).PollPortables(context.Background()); got != nil {
		t.Errorf("without a lister nothing is watched: %+v", got)
	}
}

// ---- handling an arrival ------------------------------------------------------------------

func target() Target {
	return Target{Kind: index.KindVolume, ID: "vol-1A2B3C4D", Label: "SANDISK 32GB", Arg: "E:", Display: "SANDISK 32GB (E:)"}
}

func TestHandleIndexesLogsAndNotifies(t *testing.T) {
	w := newWorld(t)
	a, _ := w.agent(t)
	a.Handle(context.Background(), target())

	if len(w.indexed) != 1 || w.indexed[0].Arg != "E:" {
		t.Fatalf("indexed = %+v", w.indexed)
	}
	if len(w.sleeps) != 1 || w.sleeps[0] != 3*time.Second {
		t.Errorf("must wait 3 s for the drive to settle, slept %v", w.sleeps)
	}
	if len(w.notified) != 1 || w.notified[0] != [2]string{"SANDISK 32GB indexed", "182,334 entries"} {
		t.Errorf("notification = %v", w.notified)
	}
	log := w.logText(t)
	for _, want := range []string{"INFO  arrived: SANDISK 32GB (E:) [vol-1A2B3C4D]", "INFO  indexed SANDISK 32GB (E:): 182,334 entries in 0ms"} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}
}

func TestHandleRespectsLocksNotificationsAndErrors(t *testing.T) {
	// Another process is indexing it: don't.
	w := newWorld(t)
	w.locked["vol-1A2B3C4D"] = true
	a, _ := w.agent(t)
	a.Handle(context.Background(), target())
	if len(w.indexed) != 0 || !strings.Contains(w.logText(t), "already being indexed") {
		t.Errorf("locked: indexed %v log %s", w.indexed, w.logText(t))
	}

	// Notifications switched off.
	w = newWorld(t)
	a, _ = w.agent(t, func(s *Settings) { s.Notifications = false })
	a.Handle(context.Background(), target())
	if len(w.indexed) != 1 || len(w.notified) != 0 {
		t.Errorf("notifications off: %v %v", w.indexed, w.notified)
	}

	// Indexing fails: logged, no pop-up.
	w = newWorld(t)
	w.indexErr["vol-1A2B3C4D"] = errors.New("the drive was removed")
	a, _ = w.agent(t)
	a.Handle(context.Background(), target())
	if len(w.notified) != 0 || !strings.Contains(w.logText(t), "ERROR indexing SANDISK 32GB (E:) failed: the drive was removed") {
		t.Errorf("failure: %v\n%s", w.notified, w.logText(t))
	}

	// A failing pop-up is logged once and harmless.
	w = newWorld(t)
	w.notifyErr = errors.New("no toast for you")
	a, _ = w.agent(t)
	a.Handle(context.Background(), target())
	a.Handle(context.Background(), target())
	if n := strings.Count(w.logText(t), "couldn't show a notification"); n != 1 {
		t.Errorf("notification failure logged %d times", n)
	}

	// Shutting down during the settle wait: nothing is indexed.
	w = newWorld(t)
	a, _ = w.agent(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.Handle(ctx, target())
	if len(w.indexed) != 0 {
		t.Error("a cancelled agent must not start indexing")
	}
}

// ---- the whole loop ------------------------------------------------------------------------

func TestRunWatchesAndIndexesOneAtATime(t *testing.T) {
	w := newWorld(t)
	w.indexWork = 30 * time.Millisecond
	a, _ := w.agent(t, func(s *Settings) {
		s.DrivePoll, s.PhonePoll, s.MediaPoll, s.Settle = 5*time.Millisecond, 5*time.Millisecond, time.Hour, 0
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()

	time.Sleep(20 * time.Millisecond) // the agent has taken its baseline
	w.plug(usbDrive("E:", "ONE", 0x01), usb)
	w.plug(usbDrive("F:", "TWO", 0x02), usb) // two arrive together
	w.mu.Lock()
	w.devs = []phone.Device{{Serial: "R58", State: "device", Model: "Pixel 7"}}
	w.mu.Unlock()

	deadline := time.Now().Add(5 * time.Second)
	for {
		w.mu.Lock()
		n := len(w.indexed)
		w.mu.Unlock()
		if n >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond) // give duplicates a chance to appear
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop")
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	counts := map[string]int{}
	for _, tg := range w.indexed {
		counts[tg.ID]++
	}
	if len(counts) != 3 || counts["vol-00000001"] != 1 || counts["vol-00000002"] != 1 || counts["adb-R58"] != 1 {
		t.Errorf("each arrival must be indexed exactly once: %v", counts)
	}
	if w.peak.Load() != 1 {
		t.Errorf("indexing must be one at a time, peak concurrency was %d", w.peak.Load())
	}
	log := w.logText(t)
	if !strings.Contains(log, "agent started") || !strings.Contains(log, "agent stopped") {
		t.Errorf("log:\n%s", log)
	}
}

func TestRunDoesCatchUpAtStartup(t *testing.T) {
	w := newWorld(t)
	w.plug(usbDrive("E:", "ALREADY HERE", 0xAB), usb) // plugged in before the agent starts, never indexed
	a, _ := w.agent(t, func(s *Settings) { s.Settle, s.DrivePoll, s.PhonePoll = 0, time.Hour, time.Hour })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		n := len(w.indexed)
		w.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if len(w.indexed) != 1 || w.indexed[0].Arg != "E:" {
		t.Errorf("indexed = %+v", w.indexed)
	}
}

func TestDefaultSettingsMatchTheSpec(t *testing.T) {
	s := DefaultSettings()
	if !s.Notifications || s.Settle != 3*time.Second || s.DrivePoll != 2*time.Second || s.PhonePoll != 5*time.Second || s.StaleAfter != 24*time.Hour {
		t.Errorf("defaults = %+v", s)
	}
}

func TestRunIndexesAPortableDeviceWhenItIsPluggedIn(t *testing.T) {
	w := newWorld(t)
	a, _ := w.agent(t, func(s *Settings) { s.Settle, s.DrivePoll, s.PhonePoll = 0, time.Hour, 5*time.Millisecond })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()

	time.Sleep(20 * time.Millisecond) // the agent is watching; now the phone is plugged in
	w.mu.Lock()
	w.ports = []portable.Device{galaxy}
	w.mu.Unlock()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		n := len(w.notified)
		w.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if len(w.indexed) != 1 || w.indexed[0].Kind != index.KindDevice || w.indexed[0].Arg != galaxy.Arg() {
		t.Errorf("indexed = %+v", w.indexed)
	}
	if len(w.notified) != 1 || w.notified[0][0] != "Galaxy S21 indexed" {
		t.Errorf("notified = %v", w.notified)
	}
}

func TestSlowUSBLinkIsReportedWhenTheDriveArrives(t *testing.T) {
	w := newWorld(t)
	w.links = map[string]volume.USBLink{`E:\`: {Speed: volume.SpeedHigh, Capable: volume.SpeedSuper, PortKnown: true}}
	a, _ := w.agent(t)
	a.Handle(context.Background(), target())

	if len(w.notified) != 2 {
		t.Fatalf("want a slow-link pop-up and an indexed pop-up, got %v", w.notified)
	}
	slow := w.notified[0]
	if slow[0] != "SANDISK 32GB is on a slow USB link" || !strings.Contains(slow[1], "480 Mbps") ||
		!strings.Contains(slow[1], "5 Gbps") || !strings.Contains(slow[1], "another port") {
		t.Errorf("slow-link pop-up = %v", slow)
	}
	if !strings.Contains(w.logText(t), "SLOW LINK: SANDISK 32GB (E:) runs at USB 2.0") {
		t.Errorf("log:\n%s", w.logText(t))
	}
	if len(w.indexed) != 1 {
		t.Error("a slow drive must still be indexed")
	}

	// Through a USB 2.0 hub, and a cable that fell back: their own advice.
	for _, c := range []struct {
		l    volume.USBLink
		want string
	}{
		{volume.USBLink{Speed: volume.SpeedHigh, Capable: volume.SpeedSuper, PortKnown: true, ViaHub: true}, "straight into the PC"},
		{volume.USBLink{Speed: volume.SpeedHigh, Capable: volume.SpeedSuper, PortKnown: true, PortTwinUSB3: true}, "fully in"},
	} {
		w := newWorld(t)
		w.links = map[string]volume.USBLink{`E:\`: c.l}
		a, _ := w.agent(t)
		a.Handle(context.Background(), target())
		if len(w.notified) == 0 || !strings.Contains(w.notified[0][1], c.want) {
			t.Errorf("%+v: pop-ups %v, want %q", c.l, w.notified, c.want)
		}
	}
}

func TestFastOrUnknownUSBLinkStaysQuiet(t *testing.T) {
	for _, l := range []volume.USBLink{
		{Speed: volume.SpeedSuper, Capable: volume.SpeedSuper, PortKnown: true, PortUSB3: true},
		{Speed: volume.SpeedHigh, Capable: volume.SpeedHigh, PortKnown: true}, // a USB 2.0 drive
		{Speed: volume.SpeedHigh}, // Windows can't tell
	} {
		w := newWorld(t)
		w.links = map[string]volume.USBLink{`E:\`: l}
		a, _ := w.agent(t)
		a.Handle(context.Background(), target())
		if len(w.notified) != 1 || !strings.HasSuffix(w.notified[0][0], "indexed") {
			t.Errorf("%+v: pop-ups %v, want only the indexed one", l, w.notified)
		}
	}
	// Notifications off: logged, no pop-up.
	w := newWorld(t)
	w.links = map[string]volume.USBLink{`E:\`: {Speed: volume.SpeedHigh, Capable: volume.SpeedSuper, PortKnown: true}}
	a, _ := w.agent(t, func(s *Settings) { s.Notifications = false })
	a.Handle(context.Background(), target())
	if len(w.notified) != 0 || !strings.Contains(w.logText(t), "SLOW LINK") {
		t.Errorf("notifications off: %v\n%s", w.notified, w.logText(t))
	}
}
