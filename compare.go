package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Comparisons of two periods: before and after an update, the previous and
// the current boot, or any two ranges. A difference in time is not proof of a
// cause, so the output says how much data stands behind each number and
// compares idle periods separately, where load does not hide a change.

var compareKeys = []string{"cpu", "cpu_temp", "mem", "swap", "psi_cpu", "psi_memory", "psi_io", "gpu", "gpu_temp", "gpu_junction", "gpu_mem_temp", "gpu_power", "gpu_fan", "system_fan", "disk_temp"}

// idle: little CPU and GPU load, where temperatures and power are comparable.
func idle(m map[string]float64) bool {
	if c, ok := m["cpu"]; !ok || c >= 15 {
		return false
	}
	if g, ok := m["gpu"]; ok && g >= 15 {
		return false
	}
	return true
}

type period struct {
	from, to    time.Time
	secs, quiet float64
	all, calm   *span
	hourly      bool // from the long-term summary: no idle split
}

func measurePeriod(dir string, from, to time.Time, interval float64) (p period, err error) {
	p = period{from: from, to: to, all: newSpan(to.Unix()), calm: newSpan(to.Unix())}
	edge := rawSince(dir)
	if from.Before(edge) {
		if err = loadLong(dir, from, minTime(to, edge), func(r Record) {
			if r.Type == "hour" {
				if part := overlap(r.T, 3600, from, minTime(to, edge)); part > 0 && r.Secs > 0 {
					w := float64(r.Secs) * part / 3600
					p.secs += w
					p.all.add(r, w)
					p.hourly = true // only when summaries were really used
				}
			}
		}); err != nil {
			return
		}
	}
	sp := &spans{fallback: interval}
	err = load(dir, maxTime(from, edge), to.Add(10*time.Minute), func(r Record) {
		switch {
		case r.Type == "sample":
			secs, _ := sp.next(r)
			part := overlap(r.T, secs, maxTime(from, edge), to)
			if part <= 0 {
				return
			}
			p.secs += part
			p.all.add(r, part)
			if idle(r.Avg) {
				p.quiet += part
				p.calm.add(r, part)
			}
		case r.Type == "event" && gapNames[r.Kind] != "" || r.Type == "event" && r.Kind == "start":
			sp.cut()
		}
	})
	return
}

func avgOf(s *span, k string) (float64, bool) {
	if w := s.wt[k]; w > 0 {
		return s.sum[k] / w, true
	}
	return 0, false
}

func printCompare(w io.Writer, title string, a, b period) {
	fmt.Fprintln(w, title)
	for _, p := range []struct {
		name string
		p    period
	}{{"before", a}, {"after", b}} {
		src := "samples"
		if p.p.hourly {
			src = "partly hourly summaries"
		}
		fmt.Fprintf(w, "  %-6s %s - %s: data for %s (%s), idle %s\n", p.name, p.p.from.Format("01-02 15:04"), p.p.to.Format("01-02 15:04"),
			fmtSecs(int64(p.p.secs)), src, fmtSecs(int64(p.p.quiet)))
	}
	if a.secs < 3600 || b.secs < 3600 {
		fmt.Fprintln(w, "  note: less than an hour of data on one side; differences may be chance")
	}
	fmt.Fprintf(w, "  %-28s %21s %23s\n", "metric (average)", "all time", "idle only")
	fmt.Fprintf(w, "  %-28s %10s %10s %11s %11s\n", "", "before", "after", "before", "after")
	for _, k := range compareKeys {
		x, ok1 := avgOf(a.all, k)
		y, ok2 := avgOf(b.all, k)
		if !ok1 || !ok2 {
			continue
		}
		l, u := label(k)
		line := fmt.Sprintf("  %-28s %10.1f %10.1f", l+" "+u, x, y)
		qx, okq1 := avgOf(a.calm, k)
		qy, okq2 := avgOf(b.calm, k)
		if okq1 && okq2 && a.quiet >= 600 && b.quiet >= 600 {
			line += fmt.Sprintf(" %11.1f %11.1f", qx, qy)
			if d := qy - qx; abs64(d) >= max(0.5, abs64(qx)*0.1) {
				line += fmt.Sprintf("  (%+.1f)", d)
			}
		} else {
			line += fmt.Sprintf(" %11s %11s", "-", "-")
		}
		fmt.Fprintln(w, line)
	}
	fmt.Fprintln(w, "  A change in the idle columns is the better sign; a difference in time is not proof of a cause.")
}

func abs64(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// cmdCompare: "update", "boots" or FROM1 TO1 FROM2 TO2.
func cmdCompare(w io.Writer, args []string) error {
	cfg, _ := loadConfig()
	dir, now := dataDir(), time.Now()
	iv := cfg.Interval.Seconds()
	const window = 48 * time.Hour
	switch {
	case len(args) == 1 && args[0] == "update":
		var at time.Time
		var what string
		if err := loadAll(dir, now.AddDate(0, 0, -keepDays), now, func(r Record) {
			if r.Type == "event" && (r.Kind == "package" || r.Kind == "fact" && r.Key == "kernel") {
				at, what = time.Unix(r.T, 0), r.Key
				if r.Kind == "fact" {
					what = "kernel " + safeName(r.Old) + " -> " + safeName(r.New)
				} else {
					what = safeName(what)
				}
			}
		}); err != nil {
			return err
		}
		if at.IsZero() {
			fmt.Fprintln(w, "No update found in the history.")
			return nil
		}
		a, err := measurePeriod(dir, at.Add(-window), at, iv)
		if err != nil {
			return err
		}
		b, err := measurePeriod(dir, at, minTime(at.Add(window), now), iv)
		if err != nil {
			return err
		}
		printCompare(w, fmt.Sprintf("Before and after the last update, %s (%s)", at.Format("2006-01-02 15:04"), what), a, b)
	case len(args) == 1 && args[0] == "boots":
		var boots []time.Time
		if err := loadAll(dir, now.AddDate(0, 0, -keepDays), now, func(r Record) {
			if r.Type == "event" && r.Kind == "boot" {
				boots = append(boots, time.Unix(r.T, 0))
			}
		}); err != nil {
			return err
		}
		sort.Slice(boots, func(i, j int) bool { return boots[i].Before(boots[j]) })
		if len(boots) < 2 {
			fmt.Fprintln(w, "Fewer than two boots in the history; compare needs the previous boot and this one.")
			return nil
		}
		p, c := boots[len(boots)-2], boots[len(boots)-1]
		a, err := measurePeriod(dir, p, c, iv)
		if err != nil {
			return err
		}
		b, err := measurePeriod(dir, c, now, iv)
		if err != nil {
			return err
		}
		printCompare(w, "The previous boot and this one", a, b)
	case len(args) == 4:
		f1, t1, err := parseRange(args[0], args[1], now)
		if err != nil {
			return err
		}
		f2, t2, err := parseRange(args[2], args[3], now)
		if err != nil {
			return err
		}
		a, err := measurePeriod(dir, f1, t1, iv)
		if err != nil {
			return err
		}
		b, err := measurePeriod(dir, f2, t2, iv)
		if err != nil {
			return err
		}
		printCompare(w, "Two ranges", a, b)
	default:
		return fmt.Errorf("usage: azor compare update | boots | FROM1 TO1 FROM2 TO2")
	}
	return nil
}

// cmdProblems counts what keeps coming back: alerts, failed timers and
// services, unrecorded shutdowns, interruptions, and desktop restarts right
// after a resume.
func cmdProblems(w io.Writer, args []string) error {
	span := []string{"30d"}
	if len(args) > 0 {
		span = args
	}
	from, to, err := since(span)
	if err != nil {
		return err
	}
	type alertStat struct {
		n     int
		secs  []int64
		peak  float64
		procs map[string]int
	}
	alerts := map[string]*alertStat{}
	runs, fails, oldFails := map[string]map[float64]bool{}, map[string]int{}, map[string]int{}
	var logouts []int64
	unitFails := map[string][]int64{}
	units, counts := map[string]int{}, map[string]int{}
	var resumes []int64
	var desk []int64
	var snaps []Record
	if err := loadAll(dataDir(), from, to, func(r Record) {
		switch {
		case r.Type == "snapshot":
			snaps = append(snaps, r)
		case r.Type != "event":
		case r.Kind == "alert" || r.Kind == "clear":
			a := alerts[r.Key]
			if a == nil {
				a = &alertStat{procs: map[string]int{}}
				alerts[r.Key] = a
			}
			if r.Kind == "alert" {
				a.n++
			} else {
				a.secs = append(a.secs, r.Secs)
				a.peak = max(a.peak, r.Val)
			}
		case r.Kind == "timer":
			if runs[r.Key] == nil {
				runs[r.Key] = map[float64]bool{}
			}
			if r.Val > 0 && !runs[r.Key][r.Val] {
				runs[r.Key][r.Val] = true
				if strings.HasPrefix(r.New, "failed") {
					fails[r.Key]++
				}
			} else if r.Val == 0 && strings.HasPrefix(r.New, "failed") {
				oldFails[r.Key]++ // older records: no run time, successes not kept
			}
		case r.Kind == "unit" && r.New != "":
			unitFails[r.Key] = append(unitFails[r.Key], r.T)
		case r.Kind == "suspend":
			counts["suspends"]++
			resumes = append(resumes, r.T)
		case r.Kind == "desktop":
			desk = append(desk, r.T)
			if r.Key == "display server" && r.New == "ended" {
				logouts = append(logouts, r.T)
			}
		case r.Kind == "unexpected_boot" || r.Kind == "interrupted" || r.Kind == "lost" || r.Kind == "boot":
			counts[r.Kind]++
		}
	}); err != nil {
		return err
	}
	atLogout := 0
	for k, ts := range unitFails { // the desktop's services stop with its session
		for _, t := range ts {
			ended := false
			for _, l := range logouts {
				if t >= l-30 && t-l <= 180 {
					ended = true
					break
				}
			}
			if ended {
				atLogout++
			} else {
				units[k]++
			}
		}
	}
	for _, s := range snaps { // which program led at the time of each alert
		for k, a := range alerts {
			if strings.Contains(s.Key, k) && len(s.CPU) > 0 {
				a.procs[s.CPU[0].Name]++
			}
		}
	}
	fmt.Fprintf(w, "Problems %s - %s\n", from.Format("2006-01-02"), to.Format("2006-01-02"))
	any := false
	for _, k := range sortedKeys(alerts) {
		a := alerts[k]
		if a.n == 0 {
			continue
		}
		any = true
		l, u := label(k)
		line := fmt.Sprintf("  alert %s: %dx", l, a.n)
		if len(a.secs) > 0 {
			sort.Slice(a.secs, func(i, j int) bool { return a.secs[i] < a.secs[j] })
			line += fmt.Sprintf(", median %s, highest %.0f %s", fmtSecs(a.secs[len(a.secs)/2]), a.peak, u)
		}
		if top, n := topOf(a.procs); n > 0 {
			line += fmt.Sprintf(", most often busiest: %s (%d of %d snapshots)", top, n, sumOf(a.procs))
		}
		fmt.Fprintln(w, line)
	}
	for _, k := range sortedKeys(runs) {
		if fails[k] > 0 {
			any = true
			fmt.Fprintf(w, "  timer %s: %d failed of %d runs\n", k, fails[k], len(runs[k]))
		}
	}
	for _, k := range sortedKeys(oldFails) {
		any = true
		fmt.Fprintf(w, "  timer %s: %d failed runs in older records (their successes were not kept, so the share is unknown)\n", k, oldFails[k])
	}
	for _, k := range sortedKeys(units) {
		any = true
		fmt.Fprintf(w, "  service %s failed %dx\n", k, units[k])
	}
	if n := counts["unexpected_boot"]; n > 0 {
		any = true
		fmt.Fprintf(w, "  %d of %d boots followed an end without a recorded shutdown\n", n, counts["boot"])
	}
	if n := counts["interrupted"]; n > 0 {
		any = true
		fmt.Fprintf(w, "  Azor was interrupted %dx\n", n)
	}
	if n := counts["lost"]; n > 0 {
		any = true
		fmt.Fprintf(w, "  history could not be written %dx\n", n)
	}
	after := 0
	for _, t := range resumes {
		for _, d := range desk {
			if d >= t && d-t <= 180 {
				after++
				break
			}
		}
	}
	if atLogout > 0 {
		fmt.Fprintf(w, "  (%d service failures at the end of a desktop session are left out: its services stop with it)\n", atLogout)
	}
	if after > 0 {
		any = true
		fmt.Fprintf(w, "  desktop restarted within 3 min after %d of %d resumes\n", after, len(resumes))
	}
	if !any {
		fmt.Fprintln(w, "  nothing recurring found")
	}
	return nil
}

func topOf(m map[string]int) (string, int) {
	best, n := "", 0
	for _, k := range sortedKeys(m) {
		if m[k] > n {
			best, n = k, m[k]
		}
	}
	return best, n
}

func sumOf(m map[string]int) (s int) {
	for _, v := range m {
		s += v
	}
	return
}
