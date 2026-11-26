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
}

func (t timer) failed() bool {
	return t.Last > 0 && t.Result != "" && t.Result != "success" && t.Result != "running" && t.Result != "unknown"
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

func readTimers(ignore []string) (out []timer, ok bool) {
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
		b, _ = systemctl(append([]string{scope, "show", "--no-pager", "--timestamp=unix", "-p", "Id,Result,ActiveState,ExecMainStartTimestamp,ExecMainExitTimestamp", "--"}, units...)...)
		info := parseShow(string(b))
		for _, t := range ts {
			m := info[t.Unit]
			t.Result = result(m)
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

// timerStates keeps only good and bad, so that normal runs are not events.
func timerStates(ts []timer) map[string]string {
	out := map[string]string{}
	for _, t := range ts {
		if t.Result == "running" || t.Result == "unknown" || t.Result == "" || t.Last == 0 {
			continue
		}
		k := t.Name
		if t.User {
			k = "user " + t.Name
		}
		out[k] = "ok"
		if t.failed() {
			out[k] = "failed: " + t.Result
		}
	}
	return out
}

// watchedUnits returns the state of units declared by the user; a unit is
// looked up in the system first, then in the user session.
func watchedUnits(names []string) map[string]string {
	if len(names) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, scope := range []string{"--user", "--system"} {
		b, ok := systemctl(append([]string{scope, "show", "--no-pager", "-p", "Id,LoadState,ActiveState,Result", "--"}, names...)...)
		if !ok {
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
	return out
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
	ts, ok := readTimers(cfg.IgnoreTimers)
	if !ok {
		return fmt.Errorf("systemd timers are not available")
	}
	at := func(t int64) string {
		if t == 0 {
			return "-"
		}
		return time.Unix(t, 0).Format("01-02 15:04")
	}
	fmt.Fprintf(w, "  %-34s %-6s %-12s %-12s %6s  %s\n", "timer", "scope", "last", "next", "took", "result")
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
		}
		if t.failed() {
			res = "FAILED: " + res
		}
		fmt.Fprintf(w, "  %-34s %-6s %-12s %-12s %5ds  %s\n", clean(t.Name), scope, at(t.Last), at(t.Next), t.Dur, clean(res))
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
