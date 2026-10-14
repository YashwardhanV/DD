package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/phone"
	"github.com/yashwardhanv/dd/internal/portable"
	"github.com/yashwardhanv/dd/internal/ui"
	"github.com/yashwardhanv/dd/internal/volume"
)

// Settings are the agent's options. (Stage 11 fills them from config.toml.)
type Settings struct {
	// Notifications shows a Windows pop-up when a drive has been indexed.
	Notifications bool
	// AutoIndex lists extra, internal drives to index automatically ("D:").
	// External drives are always indexed.
	AutoIndex []string
	// StaleAfter: at start-up, drives whose index is older than this are refreshed.
	StaleAfter time.Duration
	// Settle is how long to wait after a drive appears before reading it
	// (Windows is still mounting it and Defender is looking at it).
	Settle time.Duration
	// DrivePoll / PhonePoll / MediaPoll are the polling intervals. Portable
	// devices are checked together with phones.
	DrivePoll, PhonePoll, MediaPoll time.Duration
	// DeviceRetry is how long to wait before trying a portable device again
	// after it couldn't be read (a locked phone shows no files); every further
	// failure doubles the wait, up to DeviceRetryMax.
	DeviceRetry, DeviceRetryMax time.Duration
}

// DefaultSettings are the values BUILD.md specifies.
func DefaultSettings() Settings {
	return Settings{
		Notifications:  true,
		StaleAfter:     24 * time.Hour,
		Settle:         3 * time.Second,
		DrivePoll:      2 * time.Second,
		PhonePoll:      5 * time.Second,
		MediaPoll:      10 * time.Second,
		DeviceRetry:    30 * time.Second,
		DeviceRetryMax: 5 * time.Minute,
	}
}

// Target is something worth (re-)indexing.
type Target struct {
	Kind    string // index.KindVolume, index.KindPhone or index.KindDevice
	ID      string // the index's SourceID: "vol-1A2B3C4D", "adb-<serial>" or "dev-<12 hex>"
	Label   string // "SANDISK 32GB" - for messages
	Arg     string // what to pass to `drivedex index`: "E:", "phone:<serial>" or "device:dev-<12 hex>"
	Display string // "SANDISK 32GB (E:)" - for the log
}

// Outcome is the result of an indexing run.
type Outcome struct{ Entries int }

// Deps is everything the agent needs from the outside world. In the real
// program these are Windows calls; tests supply fakes, which is how the
// watching logic is tested without any drive.
type Deps struct {
	// Mask returns the bitmask of drive letters in use (GetLogicalDrives); a
	// change means a drive arrived or left. nil: every poll does a full check.
	Mask func() (uint32, error)
	// Volumes lists the local drives.
	Volumes func() ([]volume.Volume, error)
	// Bus says how a drive is connected.
	Bus func(root string) (volume.BusInfo, error)
	// Devices lists phones; nil when adb isn't available.
	Devices func(ctx context.Context) ([]phone.Device, error)
	// Portables lists portable devices (phones in "File transfer" mode,
	// iPhones, cameras); nil disables watching them.
	Portables func() ([]portable.Device, error)
	// SystemDrive is the drive Windows runs from ("C:"); it is never auto-indexed.
	SystemDrive string
	// Index (re-)indexes a target.
	Index func(ctx context.Context, t Target) (Outcome, error)
	// Locked reports whether another process is indexing the target right now.
	Locked func(t Target) bool
	// Header returns the saved index header for an ID, if there is one.
	Header func(id string) (index.Header, bool)
	// Notify shows a pop-up; nil disables notifications.
	Notify func(title, body string) error
	// USBLink says how fast a USB drive is linked ("E:\"); nil skips the check.
	USBLink func(root string) (volume.USBLink, error)
	// Sleep waits d, returning false if ctx ended first.
	Sleep func(ctx context.Context, d time.Duration) bool
	// Now is the clock.
	Now func() time.Time
}

// Agent watches for drives, phones and portable devices.
type Agent struct {
	d   Deps
	set Settings
	log *Logger

	mu         sync.Mutex
	mask       uint32
	maskKnown  bool
	polls      int
	known      map[string]string // ready volumes seen last time: SourceID -> letter
	phones     map[string]string // phones seen last time: serial -> state
	portables  map[string]bool   // portable devices seen last time, by SourceID
	retries    map[string]retry  // portable devices that couldn't be read, by SourceID
	loggedFail map[string]bool   // problems already logged (so a persistent one logs once)
}

// retry schedules another attempt at a portable device that couldn't be read.
type retry struct {
	at     time.Time
	delay  time.Duration
	queued bool // the attempt is waiting in (or going through) the queue
}

// New creates an Agent.
func New(d Deps, set Settings, log *Logger) *Agent {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Sleep == nil {
		d.Sleep = func(ctx context.Context, dur time.Duration) bool {
			t := time.NewTimer(dur)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return false
			case <-t.C:
				return true
			}
		}
	}
	return &Agent{d: d, set: set, log: log, known: map[string]string{}, phones: map[string]string{},
		portables: map[string]bool{}, retries: map[string]retry{}, loggedFail: map[string]bool{}}
}

// logOnce logs a problem the first time only, until clearFail resets it.
func (a *Agent) logOnce(key, format string, args ...any) {
	if !a.loggedFail[key] {
		a.loggedFail[key] = true
		a.log.Errorf(format, args...)
	}
}

func (a *Agent) clearFail(key string) { delete(a.loggedFail, key) }

// wantsAutoIndex decides whether a drive is one the agent should index by
// itself: every external drive (USB, FireWire, memory card, eSATA,
// Thunderbolt - see volume.IsExternal), and internal drives only if listed in
// AutoIndex.
func (a *Agent) wantsAutoIndex(v volume.Volume) (volume.BusInfo, bool) {
	var bus volume.BusInfo
	if a.d.Bus != nil {
		if b, err := a.d.Bus(v.Root); err == nil {
			bus = b
		} else {
			a.logOnce("bus-"+v.Letter(), "couldn't tell how %s is connected (%v); judging it by its drive type alone", v.Letter(), err)
		}
	}
	if volume.IsExternal(v, bus, a.d.SystemDrive) {
		return bus, true
	}
	for _, l := range a.set.AutoIndex {
		if letter, ok := volume.NormalizeLetter(l); ok && strings.EqualFold(letter, v.Letter()) {
			return bus, true
		}
	}
	return bus, false
}

func volumeTarget(v volume.Volume, bus volume.BusInfo) Target {
	label := strings.TrimSpace(v.Label)
	if label == "" {
		label = bus.Name()
	}
	if label == "" {
		label = "Drive " + v.Letter()
	}
	return Target{
		Kind: index.KindVolume, ID: volume.SourceID(v), Label: label, Arg: v.Letter(),
		Display: fmt.Sprintf("%s (%s)", label, v.Letter()),
	}
}

func deviceTarget(d portable.Device) Target {
	return Target{
		Kind: index.KindDevice, ID: d.SourceID(), Label: d.Label(), Arg: d.Arg(),
		Display: fmt.Sprintf("device %s", d.Label()),
	}
}

func phoneTarget(d phone.Device) Target {
	return Target{
		Kind: index.KindPhone, ID: phone.SourceID(d.Serial), Label: d.Name(), Arg: "phone:" + d.Serial,
		Display: fmt.Sprintf("phone %s (%s)", d.Name(), d.Serial),
	}
}

// readyVolumes returns the ready, identifiable volumes by SourceID.
func (a *Agent) readyVolumes() (map[string]volume.Volume, error) {
	vols, err := a.d.Volumes()
	if err != nil {
		return nil, err
	}
	out := map[string]volume.Volume{}
	for _, v := range vols {
		if v.Ready && v.Serial != 0 && v.DriveType != volume.DriveRemote {
			out[volume.SourceID(v)] = v
		}
	}
	return out, nil
}

// stale reports whether the saved index for id is missing or older than StaleAfter.
func (a *Agent) stale(id string) bool {
	if a.d.Header == nil {
		return true
	}
	h, ok := a.d.Header(id)
	return !ok || a.d.Now().Sub(h.UpdatedAt) > a.set.StaleAfter
}

// Prime records what is plugged in right now (so it is not mistaken for "just
// arrived") and returns the targets whose index is missing or stale, which the
// agent refreshes once at start-up. Without this a drive that is always
// plugged in would never be indexed by the agent at all.
func (a *Agent) Prime(ctx context.Context) []Target {
	a.mu.Lock()
	defer a.mu.Unlock()
	var catchUp []Target

	if a.d.Mask != nil {
		if m, err := a.d.Mask(); err == nil {
			a.mask, a.maskKnown = m, true
		}
	}
	if vols, err := a.readyVolumes(); err != nil {
		a.log.Errorf("couldn't list the drives at start-up: %v", err)
	} else {
		for id, v := range vols {
			a.known[id] = v.Letter()
			if bus, ok := a.wantsAutoIndex(v); ok && a.stale(id) {
				catchUp = append(catchUp, volumeTarget(v, bus))
			}
		}
	}
	if a.d.Devices != nil {
		if devs, err := a.d.Devices(ctx); err != nil {
			a.logOnce("devices", "couldn't ask adb for phones: %v", err)
		} else {
			for _, d := range devs {
				a.phones[d.Serial] = d.State
				if d.Usable() && a.stale(phone.SourceID(d.Serial)) {
					catchUp = append(catchUp, phoneTarget(d))
				}
			}
		}
	}
	if a.d.Portables != nil {
		if devs, err := a.d.Portables(); err != nil {
			a.logOnce("portables", "couldn't list the portable devices: %v", err)
		} else {
			for _, d := range devs {
				a.portables[d.SourceID()] = true
				if !a.viaADB(d) && a.stale(d.SourceID()) {
					catchUp = append(catchUp, deviceTarget(d))
				}
			}
		}
	}
	sortTargets(catchUp)
	return catchUp
}

// PollVolumes checks for newly arrived drives and returns the ones to index.
//
// A cheap check runs every time (has the set of drive letters changed?). A full
// look at every drive also runs every MediaPoll, because putting a memory card
// into a card reader doesn't change the drive letters but does change what is
// readable.
func (a *Agent) PollVolumes(ctx context.Context) []Target {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.polls++

	changed := true
	if a.d.Mask != nil {
		m, err := a.d.Mask()
		if err != nil {
			a.logOnce("mask", "couldn't read the list of drive letters: %v", err)
		} else {
			a.clearFail("mask")
			changed = !a.maskKnown || m != a.mask
			a.mask, a.maskKnown = m, true
			if every := int(a.set.MediaPoll / maxDuration(a.set.DrivePoll, time.Millisecond)); every > 0 && a.polls%every == 0 {
				changed = true // periodic media check
			}
		}
	}
	if !changed {
		return nil
	}

	vols, err := a.readyVolumes()
	if err != nil {
		a.logOnce("volumes", "couldn't list the drives: %v", err)
		return nil
	}
	a.clearFail("volumes")

	var arrivals []Target
	for id, v := range vols {
		if _, was := a.known[id]; was {
			continue
		}
		if bus, ok := a.wantsAutoIndex(v); ok {
			arrivals = append(arrivals, volumeTarget(v, bus))
		} else {
			a.log.Infof("ignoring %s (%s): internal drive (%s)", v.Letter(), nonEmpty(v.Label, volume.SourceID(v)), volume.BusName(bus.Bus))
		}
	}
	// Remember exactly what is here now: a drive that is unplugged and plugged
	// in again is "new" the second time.
	a.known = map[string]string{}
	for id, v := range vols {
		a.known[id] = v.Letter()
	}
	sortTargets(arrivals)
	return arrivals
}

// PollPhones checks adb for newly connected phones.
func (a *Agent) PollPhones(ctx context.Context) []Target {
	if a.d.Devices == nil {
		return nil
	}
	devs, err := a.d.Devices(ctx)

	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		if ctx.Err() == nil {
			a.logOnce("devices", "couldn't ask adb for phones: %v", err)
		}
		return nil
	}
	a.clearFail("devices")

	var arrivals []Target
	now := map[string]string{}
	for _, d := range devs {
		now[d.Serial] = d.State
		prev := a.phones[d.Serial]
		switch {
		case d.Usable() && prev != "device":
			arrivals = append(arrivals, phoneTarget(d))
		case d.State == "unauthorized" && prev != "unauthorized":
			a.log.Infof("phone %s (%s) is waiting for you to tap Allow on its USB debugging prompt", d.Name(), d.Serial)
		}
	}
	a.phones = now
	sortTargets(arrivals)
	return arrivals
}

// PollPortables checks for newly connected portable devices, and for devices
// whose retry time has come. It runs after PollPhones, so a phone that adb
// can already read (USB debugging on: much faster) isn't read a second way.
func (a *Agent) PollPortables(ctx context.Context) []Target {
	if a.d.Portables == nil {
		return nil
	}
	devs, err := a.d.Portables()

	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.logOnce("portables", "couldn't list the portable devices: %v", err)
		return nil
	}
	a.clearFail("portables")

	now := a.d.Now()
	var arrivals []Target
	present := map[string]bool{}
	for _, d := range devs {
		id := d.SourceID()
		present[id] = true
		if a.viaADB(d) {
			if !a.portables[id] {
				a.log.Infof("%s is connected with USB debugging: indexing it as phone %s instead", d.Label(), d.Serial)
			}
			continue
		}
		r, retrying := a.retries[id]
		switch {
		case !a.portables[id]:
			arrivals = append(arrivals, deviceTarget(d))
		case retrying && !r.queued && !now.Before(r.at):
			r.queued = true // the next failure (if any) schedules the next attempt
			a.retries[id] = r
			arrivals = append(arrivals, deviceTarget(d))
		}
	}
	// A device that is unplugged and plugged in again is new the second time,
	// and starts again from the shortest retry delay.
	for id := range a.retries {
		if !present[id] {
			delete(a.retries, id)
		}
	}
	a.portables = present
	sortTargets(arrivals)
	return arrivals
}

// viaADB reports whether adb can read this device itself (call with a.mu held).
func (a *Agent) viaADB(d portable.Device) bool {
	if d.Serial == "" {
		return false
	}
	for serial, state := range a.phones {
		if state == "device" && strings.EqualFold(serial, d.Serial) {
			return true
		}
	}
	return false
}

// deviceFailed schedules another attempt at a portable device: a phone that
// is locked, or not yet switched to "File transfer", shows no files until it is.
func (a *Agent) deviceFailed(t Target) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.retries[t.ID]
	switch {
	case r.delay == 0:
		r.delay = a.set.DeviceRetry
	case r.delay < a.set.DeviceRetryMax:
		r.delay = min(2*r.delay, a.set.DeviceRetryMax)
	}
	r.at, r.queued = a.d.Now().Add(r.delay), false
	a.retries[t.ID] = r
	a.log.Infof("will try %s again in %s (unlock it, and on a phone choose \"File transfer\")", t.Display, ui.FormatDuration(r.delay))
}

func (a *Agent) deviceSucceeded(t Target) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.retries, t.ID)
}

// Handle re-indexes one target: wait for it to settle, skip it if someone else
// is already indexing it, index it, log the result, and (optionally) show a
// notification.
func (a *Agent) Handle(ctx context.Context, t Target) {
	a.log.Infof("arrived: %s [%s]", t.Display, t.ID)
	if !a.d.Sleep(ctx, a.set.Settle) {
		return
	}
	if t.Kind == index.KindVolume {
		a.checkUSBSpeed(t)
	}
	if a.d.Locked != nil && a.d.Locked(t) {
		a.log.Infof("skipped %s: it is already being indexed by another DriveDex process", t.Display)
		return
	}
	start := a.d.Now()
	out, err := a.d.Index(ctx, t)
	if err != nil {
		if ctx.Err() != nil {
			a.log.Infof("stopped while indexing %s", t.Display)
			return
		}
		a.log.Errorf("indexing %s failed: %v", t.Display, err)
		if t.Kind == index.KindDevice {
			a.deviceFailed(t)
		}
		return
	}
	if t.Kind == index.KindDevice {
		a.deviceSucceeded(t)
	}
	took := a.d.Now().Sub(start)
	a.log.Infof("indexed %s: %s entries in %s", t.Display, ui.Thousands(int64(out.Entries)), ui.FormatDuration(took))

	if a.set.Notifications && a.d.Notify != nil {
		title := t.Label + " indexed"
		body := fmt.Sprintf("%s entries", ui.Thousands(int64(out.Entries)))
		if err := a.d.Notify(title, body); err != nil {
			a.logOnce("notify", "couldn't show a notification: %v", err)
		}
	}
}

// checkUSBSpeed tells the user, the moment a drive is plugged in, when it is
// running slower than it could - a USB 3 drive in a USB 2.0 port, behind a
// USB 2.0 hub, or on a cable that fell back to USB 2.0. Explorer never says.
func (a *Agent) checkUSBSpeed(t Target) {
	if a.d.USBLink == nil {
		return
	}
	l, err := a.d.USBLink(t.Arg + `\`)
	if err != nil || l.Speed == volume.SpeedUnknown {
		return // not USB, or Windows didn't say: nothing to report
	}
	v := l.Judge()
	if !v.Slow() {
		a.log.Infof("%s runs at %s", t.Display, l.Speed)
		return
	}
	a.log.Infof("SLOW LINK: %s runs at %s but can do %s", t.Display, l.Speed, l.Capable)
	if !a.set.Notifications || a.d.Notify == nil {
		return
	}
	fix := "Try another port (a USB 3 one)."
	switch v {
	case volume.VerdictSlowHub:
		fix = "Plug it straight into the PC, or use a USB 3 hub."
	case volume.VerdictSlowCable:
		fix = "Push it fully in; drop any extension or adapter that isn't USB 3."
	}
	title := t.Label + " is on a slow USB link"
	body := fmt.Sprintf("Running at %s; it can do %s. %s", l.Speed, l.Capable, fix)
	if err := a.d.Notify(title, body); err != nil {
		a.logOnce("notify", "couldn't show a notification: %v", err)
	}
}

// Run watches until ctx ends. Indexing happens one drive at a time on a
// single worker (several drives indexing at once would just fight over the
// bus), while polling carries on.
func (a *Agent) Run(ctx context.Context) {
	a.log.Infof("agent started")
	defer a.log.Infof("agent stopped")

	queue := make(chan Target, 32)
	var mu sync.Mutex
	pending := map[string]bool{} // IDs queued or being indexed: don't queue twice

	enqueue := func(ts []Target) {
		for _, t := range ts {
			mu.Lock()
			dup := pending[t.ID]
			if !dup {
				pending[t.ID] = true
			}
			mu.Unlock()
			if dup {
				continue
			}
			select {
			case queue <- t:
			default:
				a.log.Errorf("too many drives waiting; skipping %s", t.Display)
				mu.Lock()
				delete(pending, t.ID)
				mu.Unlock()
			}
		}
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-queue:
				a.Handle(ctx, t)
				mu.Lock()
				delete(pending, t.ID)
				mu.Unlock()
			}
		}
	}()

	enqueue(a.Prime(ctx))

	driveTick := time.NewTicker(maxDuration(a.set.DrivePoll, time.Millisecond))
	phoneTick := time.NewTicker(maxDuration(a.set.PhonePoll, time.Millisecond))
	defer driveTick.Stop()
	defer phoneTick.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-driveTick.C:
			enqueue(a.PollVolumes(ctx))
		case <-phoneTick.C:
			enqueue(a.PollPhones(ctx))
			enqueue(a.PollPortables(ctx))
		}
	}
	wg.Wait()
}

func sortTargets(ts []Target) {
	for i := 1; i < len(ts); i++ { // tiny slices: insertion sort keeps output stable
		for j := i; j > 0 && ts[j].ID < ts[j-1].ID; j-- {
			ts[j], ts[j-1] = ts[j-1], ts[j]
		}
	}
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
