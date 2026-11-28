package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// timer is one scheduled job. Any job with a systemd timer is covered:
// snapshots, backups, scrubs, trims or the user's own scripts.
type timer struct {
	Name   string `json:"name"`
	Unit   string `json:"unit"`
	User   bool   `json:"user,omitempty"`
	Last   int64  `json:"last,omitempty"`
	Next   int64  `json:"next,omitempty"`
	Result string `json:"result,omitempty"`
	Dur    int64  `json:"dur,omitempty"` // seconds of the last run
	// Stale: the result could not be read now; this is the last known one.
	Stale bool `json:"stale,omitempty"`
	// Before: systemd no longer tells this run's result; this one was saved earlier.
	Before bool `json:"before,omitempty"`
}

func (t timer) failed() bool {
	return t.Last > 0 && t.Result != "" && t.Result != "success" && t.Result != "running" && t.Result != "unknown" && t.Result != "unavailable" && t.Result != "other run"
}

func systemctl(args ...string) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "systemctl", args...).Output()
	return b, err == nil
}

// parseTimers reads `systemctl list-timers -o json`.
func parseTimers(b []byte, user bool) (out []timer) {
	var rows []struct {
		Next, Last      any
		Unit, Activates string
	}
	if json.Unmarshal(b, &rows) != nil {
		return
	}
	sec := func(v any) int64 {
		f, _ := v.(float64)
		if f <= 0 || f > 1e17 {
			return 0
		}
		return int64(f / 1e6)
	}
	for _, r := range rows {
		if unitName.MatchString(r.Unit) && unitName.MatchString(r.Activates) {
			out = append(out, timer{Name: strings.TrimSuffix(r.Unit, ".timer"), Unit: r.Activates, User: user, Last: sec(r.Last), Next: sec(r.Next)})
		}
	}
	return
}

// parseShow reads blocks of `systemctl show`, keyed by unit.
func parseShow(s string) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, block := range strings.Split(s, "\n\n") {
		m := map[string]string{}
		for _, l := range strings.Split(block, "\n") {
			if k, v, ok := strings.Cut(l, "="); ok {
				m[k] = v
			}
		}
		if m["Id"] != "" {
			out[m["Id"]] = m
		}
	}
	return out
}

func result(m map[string]string) string {
	switch m["ActiveState"] {
	case "activating", "active", "reloading":
		return "running"
	}
	// After a boot systemd reports "success" for a unit that has not run yet.
	if m["Result"] == "success" && m["ExecMainExitTimestamp"] == "" {
		return "unknown"
	}
	return m["Result"]
}

// A scope that cannot be listed is named in unknown; a result that cannot be
// read is "unavailable".
func readTimers(ignore []string) (out []timer, ok bool, unknown []string) {
	skip := map[string]bool{}
	for _, n := range ignore {
		skip[strings.TrimSuffix(n, ".timer")] = true
	}
	for _, user := range []bool{false, true} {
		scope := "--system"
		if user {
			scope = "--user"
		}
		b, good := systemctl(scope, "list-timers", "--all", "--no-pager", "-o", "json")
		if !good {
			unknown = append(unknown, strings.TrimPrefix(scope, "--")+" timers")
			continue
		}
		ok = true
		var ts []timer
		var units []string
		for _, t := range parseTimers(b, user) {
			if !skip[t.Name] {
				ts = append(ts, t)
				units = append(units, t.Unit)
			}
		}
		if len(units) == 0 {
			continue
		}
		b, shown := systemctl(append([]string{scope, "show", "--no-pager", "--timestamp=unix", "-p", "Id,Result,ActiveState,InactiveExitTimestamp,ExecMainStartTimestamp,ExecMainExitTimestamp", "--"}, units...)...)
		info := parseShow(string(b))
		for _, t := range ts {
			m := info[t.Unit]
			t.Result = result(m)
			if !shown {
				t.Result = "unavailable"
			}
			var started int64
			fmt.Sscanf(m["InactiveExitTimestamp"], "@%d", &started)
			if !fromTrigger(t.Last, started) && t.Result != "unknown" && t.Result != "unavailable" && t.Result != "running" {
				// another timer or a manual start ran the service later: its
				// result says nothing about this timer's run
				t.Result = "other run"
			}
			var a, z int64
			fmt.Sscanf(m["ExecMainStartTimestamp"], "@%d", &a)
			fmt.Sscanf(m["ExecMainExitTimestamp"], "@%d", &z)
			if a > 0 && z >= a {
				t.Dur = z - a
			}
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return
}

// fromTrigger tells whether the service started with this trigger. systemd
// starts it right away; a start much later belongs to another trigger.
func fromTrigger(last, started int64) bool {
	return started == 0 || last == 0 || (started >= last-60 && started <= last+60)
}

// mergeTimers keeps what was known before for what cannot be read now: the
// timers of a scope that did not answer and results that could not be read.
const timersFile = "timers.json"

// run is the known result of one run of a timer, identified by its start.
type run struct {
	State string `json:"state"` // "ok" or "failed: ..."
	Last  int64  `json:"last"`
}

// saveRuns keeps the known results over a reboot, when systemd forgets them.
func saveRuns(dir string, m map[string]run) error {
	if len(m) == 0 {
		return nil
	}
	return writeAtomic(filepath.Join(dir, timersFile), m)
}

// knownRuns reads the saved results and adds the newer ones found in the
// history: written after the file, or all of it when there is no file yet.
// Results without the time of their run (older versions) are not used.
func knownRuns(dir string, now time.Time, days int) map[string]run {
	m := map[string]run{}
	from := now.AddDate(0, 0, -days)
	p := filepath.Join(dir, timersFile)
	if b, err := os.ReadFile(p); err == nil {
		var raw map[string]json.RawMessage
		if json.Unmarshal(b, &raw) == nil {
			for k, v := range raw {
				var r run
				if json.Unmarshal(v, &r) == nil && r.Last > 0 && r.State != "" {
					m[k] = r
				}
			}
		}
		if i, err := os.Stat(p); err == nil {
			from = i.ModTime().Add(-time.Minute)
		}
	}
	load(dir, from, now, func(r Record) {
		if r.Type == "event" && r.Kind == "timer" && r.New != "" && r.Val > 0 {
			if last := int64(r.Val); last >= m[r.Key].Last {
				m[r.Key] = run{r.New, last}
			}
		}
	})
	return m
}

type runStat struct{ runs, failed int }

// runHistory counts the runs Azor saw per timer, each run once.
func runHistory(dir string, from, to time.Time) map[string]runStat {
	seen, out := map[string]map[float64]bool{}, map[string]runStat{}
	loadAll(dir, from, to, func(r Record) {
		if r.Type != "event" || r.Kind != "timer" || r.Val <= 0 || r.New == "" {
			return
		}
		if seen[r.Key] == nil {
			seen[r.Key] = map[float64]bool{}
		}
		if seen[r.Key][r.Val] {
			return
		}
		seen[r.Key][r.Val] = true
		s := out[r.Key]
		s.runs++
		if strings.HasPrefix(r.New, "failed") {
			s.failed++
		}
		out[r.Key] = s
	})
	return out
}

func timerKey(t timer) string {
	if t.User {
		return "user " + t.Name
	}
	return t.Name
}

// mergeTimers keeps what was known for what cannot be read now. A saved
// result is used only for the very run it belongs to.
func mergeTimers(prev, now []timer, unknown []string, known map[string]run) (out []timer) {
	old := map[string]timer{}
	for _, t := range prev {
		old[fmt.Sprint(t.User, t.Name)] = t
	}
	gone := map[bool]bool{}
	for _, u := range unknown {
		gone[strings.HasPrefix(u, "user")] = true
	}
	for _, t := range now {
		if k, ok := known[timerKey(t)]; ok && (t.Result == "unknown" || t.Result == "other run") && t.Last > 0 && k.Last == t.Last {
			switch s := k.State; {
			case s == "ok":
				t.Result, t.Before = "success", true
			case strings.HasPrefix(s, "failed: "):
				t.Result, t.Before = strings.TrimPrefix(s, "failed: "), true
			}
		}
		if t.Result == "unavailable" {
			if p, ok := old[fmt.Sprint(t.User, t.Name)]; ok && p.Result != "" && p.Result != "unavailable" {
				t.Result, t.Stale = p.Result, true
			}
		}
		out = append(out, t)
	}
	for _, t := range prev {
		if gone[t.User] {
			t.Stale = true
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return
}

// unsure lists timers whose result is not known now.
func unsure(ts []timer) (out []string) {
	for _, t := range ts {
		if t.Stale || t.Result == "unavailable" || t.Result == "other run" || (t.Result == "unknown" && t.Last > 0) {
			out = append(out, t.Name)
		}
	}
	return
}

// timerStates keeps only good and bad, so that normal runs are not events.
func timerStates(ts []timer) map[string]string {
	out := map[string]string{}
	for _, t := range ts {
		if t.Result == "running" || t.Result == "unknown" || t.Result == "unavailable" || t.Result == "other run" || t.Result == "" || t.Last == 0 || t.Stale || t.Before {
			continue
		}
		k := timerKey(t)
		out[k] = "ok"
		if t.failed() {
			out[k] = "failed: " + t.Result
		}
	}
	return out
}

// watchedUnits returns the state of units declared by the user; a unit is
// looked up in the system first, then in the user session.
func watchedUnits(names []string) (out map[string]string, complete bool) {
	if len(names) == 0 {
		return nil, true
	}
	out, complete = map[string]string{}, true
	for _, scope := range []string{"--user", "--system"} {
		b, ok := systemctl(append([]string{scope, "show", "--no-pager", "-p", "Id,LoadState,ActiveState,Result", "--"}, names...)...)
		if !ok {
			complete = false
			continue
		}
		for id, m := range parseShow(string(b)) {
			if m["LoadState"] == "loaded" {
				s := m["ActiveState"]
				if r := m["Result"]; r != "" && r != "success" {
					s += " (" + r + ")"
				}
				out[id] = s
			}
		}
	}
	return
}

// stableName describes a partition by the model of its disk, because the
// kernel may number disks differently at every boot.
func (s *sensors) stableName(dev string) string {
	for _, d := range s.glob("/sys/block/*/" + dev) {
		disk := filepath.Base(filepath.Dir(d))
		model := readStr(filepath.Dir(d) + "/device/model")
		same := 0
		for _, m := range s.glob("/sys/block/*/device/model") {
			if readStr(m) == model {
				same++
			}
		}
		if model != "" && same == 1 {
			return clean(model) + " part " + strings.TrimPrefix(strings.TrimPrefix(dev, disk), "p")
		}
	}
	return dev
}

// fsFacts reports error counters of filesystems and degraded arrays. The
// counters never reset by themselves, so only a change is of interest.
func (s *sensors) fsFacts(put func(k, v string)) {
	for _, f := range s.glob("/sys/fs/btrfs/*/devinfo/*/error_stats") {
		id := filepath.Base(filepath.Dir(f))
		fs := filepath.Dir(filepath.Dir(filepath.Dir(f)))
		devs, _ := filepath.Glob(fs + "/devices/*")
		name := "device " + id
		if len(devs) == 1 {
			name = s.stableName(filepath.Base(devs[0]))
		} else if len(devs) > 0 {
			name = s.stableName(filepath.Base(devs[0])) + " device " + id
		}
		var bad []string
		valid := false
		for _, l := range strings.Split(readStr(f), "\n") {
			p := strings.Fields(l)
			if len(p) != 2 {
				continue
			}
			if _, err := strconv.ParseUint(p[1], 10, 64); err != nil {
				valid = false // the kernel says the counters cannot be trusted
				break
			}
			valid = true
			if p[1] != "0" {
				bad = append(bad, strings.TrimSuffix(p[0], "_errs")+" "+p[1])
			}
		}
		switch {
		case !valid:
			bad = []string{"unavailable"}
		case len(bad) == 0:
			bad = []string{"none"}
		}
		put("btrfs_errors "+name, strings.Join(bad, ", "))
	}
	for _, f := range s.glob("/sys/fs/ext4/*/errors_count") {
		if v := readStr(f); v != "" {
			put("ext4_errors "+s.stableName(filepath.Base(filepath.Dir(f))), v)
		}
	}
	for _, f := range s.glob("/sys/block/md*/md/degraded") {
		put("raid_degraded "+filepath.Base(filepath.Dir(filepath.Dir(f))), readStr(f))
	}
}

func cmdTimers(w io.Writer) error {
	cfg, _ := loadConfig()
	ts, ok, unk := readTimers(cfg.IgnoreTimers)
	if !ok {
		return fmt.Errorf("systemd timers are not available")
	}
	for _, u := range unk {
		fmt.Fprintln(w, "UNKNOWN:", u, "could not be read")
	}
	ts = mergeTimers(nil, ts, nil, knownRuns(dataDir(), time.Now(), cfg.Retention)) // results saved before the boot
	at := func(t int64) string {
		if t == 0 {
			return "-"
		}
		return time.Unix(t, 0).Format("01-02 15:04")
	}
	hist := runHistory(dataDir(), time.Now().AddDate(0, 0, -30), time.Now())
	fmt.Fprintf(w, "  %-34s %-6s %-12s %-12s %6s  %-16s %s\n", "timer", "scope", "last", "next", "took", "30 days", "result")
	for _, t := range ts {
		scope, res := "system", t.Result
		if t.User {
			scope = "user"
		}
		if t.Last == 0 {
			res = "not run yet"
		}
		if t.Result == "unknown" {
			res = "unknown (not run since boot)"
			if t.Last > 0 {
				res = "UNKNOWN: ran before boot, result not saved"
			}
		}
		if t.Before {
			res += " (saved earlier)"
		}
		if t.Result == "unavailable" {
			res = "UNKNOWN: could not be read"
		}
		if t.Result == "other run" {
			res = "UNKNOWN: the service last ran from another start"
		}
		if t.failed() {
			res = "FAILED: " + res
		}
		h := hist[timerKey(t)]
		stat := "-"
		if h.runs > 0 {
			stat = fmt.Sprintf("%d run%s, %d failed", h.runs, map[bool]string{true: "", false: "s"}[h.runs == 1], h.failed)
		}
		fmt.Fprintf(w, "  %-34s %-6s %-12s %-12s %5ds  %-16s %s\n", clean(t.Name), scope, at(t.Last), at(t.Next), t.Dur, stat, clean(res))
	}
	return nil
}

// cmdInit writes a commented user configuration and never overwrites one.
func cmdInit(w io.Writer) error {
	p := configPaths()[1]
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if os.IsExist(err) {
		fmt.Fprintln(w, short(p), "already exists; nothing was changed")
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = f.WriteString(sampleConfig); err != nil {
		f.Close()
		return err
	}
	fmt.Fprintln(w, "created", short(p), "- edit it, then: systemctl --user restart azor")
	return f.Close()
}
