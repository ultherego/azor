package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A session is one run of the daemon within one boot. Saving it lets the next
// start tell a reboot, a shutdown, a stop of Azor and an interruption apart,
// without root and without asking logind.

const sessionFile = "session.json"

type runInfo struct {
	Boot     string `json:"boot"`
	Start    int64  `json:"start"`
	Last     int64  `json:"last"`
	Stopped  bool   `json:"stopped,omitempty"`
	Shutdown bool   `json:"shutdown,omitempty"`
}

// procRoot can point tests at a fake /proc; only the boot id and the boot
// clock are read from it.
func procRoot() string {
	if r := os.Getenv("AZOR_TEST_PROC"); r != "" {
		return r
	}
	return "/proc"
}

func bootID() string { return readStr(procRoot() + "/sys/kernel/random/boot_id") }

// bootClock is CLOCK_BOOTTIME in seconds: unlike Go's monotonic clock, it
// keeps running while the computer is suspended.
func bootClock() float64 {
	f := strings.Fields(readStr(procRoot() + "/uptime"))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

func loadSession(dir string) (s runInfo, ok bool) {
	b, err := os.ReadFile(filepath.Join(dir, sessionFile))
	if err != nil || json.Unmarshal(b, &s) != nil || s.Boot == "" {
		return runInfo{}, false
	}
	return s, true
}

func saveSession(dir string, s runInfo) error {
	return writeAtomic(filepath.Join(dir, sessionFile), s)
}

// startEvents explains the time before this start. last is the newest
// stored sample, which may be newer than the saved session.
func startEvents(prev runInfo, known bool, boot string, now, last int64, gapAfter int64) (out []Record) {
	since := max(prev.Last, last)
	gap := int64(0)
	if since > 0 && now > since {
		gap = now - since
	}
	ev := func(kind string, secs int64) {
		out = append(out, Record{T: now, Type: "event", Kind: kind, Secs: secs})
	}
	stopped := func(secs int64) { // the cause of this gap is known
		out = append(out, Record{T: now, Type: "event", Kind: "gap", Key: "stopped", Secs: secs})
	}
	switch {
	case !known: // first start, or a version before sessions: cause unknown
		if gap > gapAfter {
			ev("gap", gap)
		}
	case prev.Boot != boot:
		if !prev.Stopped {
			// Nothing was written at the end of the last boot: power loss,
			// a hard reset, a freeze, or Azor stopped without a chance to
			// write. It is not proof of a crash.
			ev("unexpected_boot", 0)
		}
		ev("boot", gap)
	case !prev.Stopped:
		ev("interrupted", gap) // same boot, Azor ended without stopping
	case gap > gapAfter:
		stopped(gap) // Azor was stopped on purpose, e.g. at logout
	}
	ev("start", 0)
	return
}

// shuttingDown tells whether the whole system is going down.
func shuttingDown() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	b, _ := exec.CommandContext(ctx, "systemctl", "--system", "is-system-running").Output()
	return strings.TrimSpace(string(b)) == "stopping"
}

// sleepWatch measures suspends: the boot clock keeps running in suspend, the
// monotonic clock does not.
type sleepWatch struct {
	boot float64
	mono time.Time
}

func newSleepWatch() sleepWatch { return sleepWatch{bootClock(), time.Now()} }

// slept returns how long the computer was suspended since the last call.
func (w *sleepWatch) slept() time.Duration {
	b, m := bootClock(), time.Now()
	d := (b - w.boot) - m.Sub(w.mono).Seconds()
	w.boot, w.mono = b, m
	if b == 0 || d < 3 { // too short to tell from timer jitter
		return 0
	}
	return time.Duration(d * float64(time.Second))
}

func fmtSecs(s int64) string {
	return (time.Duration(s) * time.Second).String()
}
