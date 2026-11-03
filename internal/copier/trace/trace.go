// Package trace measures where a copy spends its time.
//
// "Copying is slow" can mean many different things: the source took ages to
// list, the USB link is simply full, creating thousands of little files is
// the slow part (antivirus scans each new file), or a hard disk is seeking.
// Each one has a different fix, so before changing anything DriveDex v2
// measures. A Recorder adds up the time spent in each phase of a copy
// (listing, opening, creating, writing, renaming...) across every worker,
// counts files and bytes, and keeps a histogram of how long each file took.
// At the end Report turns that into a table plus a few plain-English hints.
//
// A nil *Recorder is valid and does nothing, so the copy engine can call it
// unconditionally: when --trace is off the cost is one nil check per call.
package trace

import (
	"math/bits"
	"sync"
	"sync/atomic"
	"time"
)

// Phase is one step of copying a file.
type Phase uint8

const (
	PlanList    Phase = iota // listing the source: walk, find, or reading the index
	PlanDst                  // checking what already exists at the destination
	SrcOpen                  // opening the source (and reading its size/attributes)
	DstCreate                // creating <name>.ddxpart
	Prealloc                 // reserving space
	Data                     // reading + writing the bytes
	SetTime                  // setting the modification time
	Close                    // closing the destination (Windows flushes metadata here)
	Rename                   // .ddxpart -> real name
	Verify                   // --verify re-reads
	PhoneList                // find / delta find on the phone
	PhoneStream              // waiting for bytes from adb (tar or pull)
	Place                    // phone: staging -> final name
	NumPhases
)

// phaseNames are the labels used in the report (and the JSON keys).
var phaseNames = [NumPhases]string{
	PlanList:    "listing the source",
	PlanDst:     "checking the destination",
	SrcOpen:     "opening source files",
	DstCreate:   "creating files (.ddxpart)",
	Prealloc:    "reserving space",
	Data:        "data (read + write)",
	SetTime:     "setting times",
	Close:       "closing files",
	Rename:      "renaming into place",
	Verify:      "verifying",
	PhoneList:   "listing on the phone",
	PhoneStream: "waiting for the phone",
	Place:       "placing phone files",
}

// String is the phase's label.
func (p Phase) String() string {
	if p < NumPhases {
		return phaseNames[p]
	}
	return "?"
}

// Mark is a point in time (monotonic nanoseconds since the process started
// measuring). The zero Mark means "not measured".
type Mark struct{ t int64 }

// base anchors Marks: time.Since uses the monotonic clock, so a change of the
// wall clock during a copy can't produce negative durations.
var base = time.Now()

func now() int64 { return int64(time.Since(base)) }

// histBuckets is the number of per-file time buckets: bucket i holds files
// that took [2^(i-1), 2^i) microseconds (bucket 0: under 1 µs); the last
// bucket also holds everything slower.
const histBuckets = 24

// Recorder collects timings. All methods are safe to call from many
// goroutines at once, and on a nil *Recorder (they then do nothing).
type Recorder struct {
	ns      [NumPhases]atomic.Int64 // total nanoseconds per phase, summed over workers
	count   [NumPhases]atomic.Int64 // how many times each phase ran
	perFile [histBuckets]atomic.Int64
	bytes   atomic.Int64
	files   atomic.Int64

	started   int64        // when the Recorder was made
	firstFile atomic.Int64 // when the first file finished (0 = none yet)

	mu    sync.Mutex
	notes []string // events worth showing, e.g. "workers 8 -> 10: +7% files/s"
}

// New returns a Recorder whose clock starts now.
func New() *Recorder { return &Recorder{started: now()} }

// Start begins timing a phase. Pair it with End.
func (r *Recorder) Start() Mark {
	if r == nil {
		return Mark{}
	}
	return Mark{now()}
}

// End adds the time since m to phase p.
func (r *Recorder) End(p Phase, m Mark) {
	if r == nil || p >= NumPhases {
		return
	}
	r.ns[p].Add(now() - m.t)
	r.count[p].Add(1)
}

// Add adds an already-measured duration to phase p (for phases measured
// elsewhere, such as a listing done before the Recorder existed).
func (r *Recorder) Add(p Phase, d time.Duration) {
	if r == nil || p >= NumPhases {
		return
	}
	r.ns[p].Add(int64(d))
	r.count[p].Add(1)
}

// FileDone records one finished file: how long it took in total and its size.
func (r *Recorder) FileDone(total time.Duration, bytes int64) {
	if r == nil {
		return
	}
	r.files.Add(1)
	r.bytes.Add(bytes)
	r.perFile[bucket(total)].Add(1)
	r.firstFile.CompareAndSwap(0, max(now()-r.started, 1))
}

// Note records an event for the report (kept in order, at most 50).
func (r *Recorder) Note(s string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if len(r.notes) < 50 {
		r.notes = append(r.notes, s)
	}
	r.mu.Unlock()
}

// bucket maps a duration to its histogram bucket (log2 of microseconds).
func bucket(d time.Duration) int {
	us := d.Microseconds()
	if us <= 0 {
		return 0
	}
	b := bits.Len64(uint64(us)) // 1 µs -> 1, 2-3 µs -> 2, 4-7 µs -> 3, ...
	if b >= histBuckets {
		return histBuckets - 1
	}
	return b
}

// bucketUpper is the slowest time a file in bucket b can have taken.
func bucketUpper(b int) time.Duration {
	if b <= 0 {
		return time.Microsecond
	}
	return time.Duration(uint64(1)<<uint(b)) * time.Microsecond
}

// Context is what the hint rules need to know about the copy.
type Context struct {
	SrcKind, DstKind string  // "usb-flash", "usb-hdd", "ssd", "hdd", "phone", "unknown"
	LinkMBps         float64 // from the USB link check; 0 = unknown
	Workers          int
	PlanSource       string  // "walk", "index", "usn+index", "phone-find", "phone-index"
	PhoneMode        string  // "tar", "pull" ("" = not a phone copy)
	DriveBenchMBps   float64 // the source drive's measured speed (doctor --bench); 0 = unknown
	Destination      string  // shown in the antivirus hint
}
