package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// A snapshot records who was using the computer at the moment of an alert.
// The regular process list is taken once a minute and can miss a short-lived
// culprit, so an alert takes its own measurement over one second.

// Defaults; azor.conf can change them (snapshot.cooldown, snapshot.followup,
// snapshot.top, snapshot.retention).
const (
	snapCooldown = time.Minute      // at most one snapshot per minute
	snapFollowUp = 10 * time.Second // a second look: is it still going on?
	snapTop      = 10
	snapKeep     = keepHours // days in the long-term history
)

type snapshots struct {
	cooldown, followUp time.Duration // followUp 0: no second look
	last               time.Time
	followAt           time.Time
	reason             string
}

func newSnapshots(c Config) *snapshots {
	return &snapshots{cooldown: c.SnapCooldown, followUp: c.SnapFollowUp}
}

// due tells whether an alert or a pending follow-up asks for a snapshot now.
func (s *snapshots) due(now time.Time, alerts []string) (reason string, ok bool) {
	if !s.followAt.IsZero() && !now.Before(s.followAt) {
		s.followAt = time.Time{}
		return fmt.Sprintf("%s (%s later)", s.reason, shortDur(s.followUp)), true
	}
	if len(alerts) == 0 || now.Sub(s.last) < s.cooldown {
		return "", false
	}
	s.last, s.reason = now, strings.Join(alerts, ", ")
	if s.followUp > 0 {
		s.followAt = now.Add(s.followUp)
	}
	return s.reason, true
}

// take measures processes and services over one second.
func takeSnapshot(t time.Time, reason string, st State, withServices bool, top int) Record {
	ps, svc, sys := &procScan{}, &svcScan{}, &svcScan{}
	ps.top(0, top, nil)
	if withServices {
		svc.scan(userCgroup(), 0)
		sys.scan(systemCgroup, 0)
	}
	start := time.Now()
	time.Sleep(time.Second)
	secs := time.Since(start).Seconds()
	r := Record{T: t.Unix(), Type: "snapshot", Key: reason, MemDef: 3, Avg: st.Values, New: strings.Join(st.Alerts, ", "), Desktop: st.Desktop}
	r.CPU, r.Mem, _ = ps.top(secs, top, nil)
	if withServices {
		r.Services = notable(svc.scan(userCgroup(), secs), top)
		r.System = notable(sys.scan(systemCgroup, secs), 3)
	}
	return r
}

func printSnapshot(w io.Writer, r Record) {
	fmt.Fprintf(w, "Snapshot %s, cause: %s\n", time.Unix(r.T, 0).Format("2006-01-02 15:04:05"), r.Key)
	if r.New != "" {
		fmt.Fprintln(w, "Active alerts:", r.New)
	}
	var vals []string
	for _, k := range []string{"cpu", "cpu_temp", "mem", "swap", "psi_cpu", "psi_memory", "psi_io", "gpu", "gpu_temp", "gpu_junction", "gpu_mem_temp", "gpu_power", "gpu_vram_mb", "disk_temp"} {
		if v, ok := r.Avg[k]; ok {
			l, u := label(k)
			vals = append(vals, fmt.Sprintf("%s %.1f %s", l, v, u))
		}
	}
	if len(vals) > 0 {
		fmt.Fprintln(w, "Readings:", strings.Join(vals, "; "))
	}
	fmt.Fprintln(w, "CPU over 1 s (1 thread = 100%):", procList(r.CPU))
	fmt.Fprintln(w, "RAM:", procList(r.Mem))
	for _, g := range []struct {
		name string
		ps   []proc
	}{{"User services", r.Services}, {"System services", r.System}, {"Desktop", r.Desktop}} {
		if len(g.ps) == 0 {
			continue
		}
		fmt.Fprintln(w, g.name)
		for _, p := range g.ps {
			fmt.Fprintf(w, "  %-28s CPU %3.0f%%  RAM %5.0f MB%s\n", p.Name, p.CPU, p.MB, p.extras())
		}
	}
}

// cmdSnapshots lists snapshots, or shows one: "latest", its number in the
// list, or the time HH:MM nearest to it.
func cmdSnapshots(w io.Writer, args []string) error {
	span, pick := []string{"7d"}, ""
	for _, a := range args {
		if _, err := parseSpan(a); err == nil {
			span = []string{a}
		} else {
			pick = a
		}
	}
	from, to, err := since(span)
	if err != nil {
		return err
	}
	var list []Record
	if err := loadAll(dataDir(), from, to, func(r Record) {
		if r.Type == "snapshot" {
			list = append(list, r)
		}
	}); err != nil {
		return err
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].T < list[j].T })
	if len(list) == 0 {
		fmt.Fprintln(w, "No snapshots. They are taken when an alert starts.")
		return nil
	}
	if pick == "" {
		for i, r := range list {
			fmt.Fprintf(w, "%3d  %s  %s\n", i+1, time.Unix(r.T, 0).Format("01-02 15:04:05"), r.Key)
		}
		fmt.Fprintln(w, "Show one: azor snapshot latest | N | HH:MM")
		return nil
	}
	var n int
	switch {
	case pick == "latest":
		n = len(list)
	case strings.Contains(pick, ":"):
		at, err := parseTime(pick, time.Now())
		if err != nil {
			return err
		}
		best := int64(-1)
		for i, r := range list {
			if d := abs(r.T - at.Unix()); best < 0 || d < best {
				best, n = d, i+1
			}
		}
	default:
		if _, err := fmt.Sscan(pick, &n); err != nil {
			return fmt.Errorf("unknown snapshot %q", pick)
		}
	}
	if n < 1 || n > len(list) {
		return fmt.Errorf("no snapshot %d (there are %d)", n, len(list))
	}
	printSnapshot(w, list[n-1])
	return nil
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// shortDur writes whole seconds under a minute as "10 s", as before.
func shortDur(d time.Duration) string {
	if d < time.Minute && d%time.Second == 0 {
		return fmt.Sprintf("%d s", d/time.Second)
	}
	return d.String()
}
