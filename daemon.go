package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
	"time"
)

// rule raises an alert at On and clears it at Off, after Hold samples in a row.
type rule struct {
	Key     string
	On, Off float64
	Hold    int
}

// makeRules derives temperature limits from the critical values reported by
// the devices; generic fallbacks apply only when a device reports none.
func makeRules(c Config, crit map[string]float64) (out []rule) {
	add := func(key string, on, span float64, hold int) {
		if v, ok := c.Alerts[key]; ok {
			if v <= 0 {
				return // switched off by the user
			}
			on = v
		}
		out = append(out, rule{key, on, on - span, hold})
	}
	temp := func(key string, fallback float64) {
		on := fallback
		if v := crit[key]; v > 0 {
			on = v - 10
		}
		add(key, on, 7, 2)
	}
	temp("cpu_temp", 90)
	temp("gpu_temp", 90)
	temp("gpu_junction", 100)
	temp("gpu_mem_temp", 98)
	temp("disk_temp", 75)
	add("mem", 92, 7, 2)
	add("psi_memory", 10, 7, 2)
	add("psi_io", 40, 25, 3)
	return
}

type alert struct {
	since  int64
	peak   float64
	streak int
	active bool
}

type alerts struct {
	rules []rule
	m     map[string]*alert
}

// update returns the events caused by one stored sample.
func (a alerts) update(t int64, avg, max map[string]float64) (out []Record) {
	for _, r := range a.rules {
		v, ok := avg[r.Key]
		if !ok {
			continue
		}
		s := a.m[r.Key]
		if s == nil {
			s = &alert{}
			a.m[r.Key] = s
		}
		if s.active && max[r.Key] > s.peak {
			s.peak = max[r.Key]
		}
		if cross := (!s.active && v >= r.On) || (s.active && v < r.Off); !cross {
			s.streak = 0
			continue
		}
		if s.streak++; s.streak < r.Hold {
			continue
		}
		s.streak = 0
		s.active = !s.active
		if s.active {
			s.since, s.peak = t, max[r.Key]
			out = append(out, Record{T: t, Type: "event", Kind: "alert", Key: r.Key, Val: round1(v)})
		} else {
			out = append(out, Record{T: t, Type: "event", Kind: "clear", Key: r.Key, Val: round1(s.peak), Secs: t - s.since})
		}
	}
	return
}

type agg struct {
	sum, max map[string]float64
	n        map[string]int
}

func newAgg() *agg {
	return &agg{map[string]float64{}, map[string]float64{}, map[string]int{}}
}

func (a *agg) add(m map[string]float64) {
	for k, v := range m {
		if a.n[k] == 0 || v > a.max[k] {
			a.max[k] = v
		}
		a.sum[k] += v
		a.n[k]++
	}
}

func (a *agg) result() (avg, max map[string]float64) {
	avg, max = map[string]float64{}, map[string]float64{}
	for k, n := range a.n {
		avg[k] = round1(a.sum[k] / float64(n))
		if m := round1(a.max[k]); m != avg[k] { // store a maximum only when it adds information
			max[k] = m
		}
	}
	return
}

// withMax fills in maxima that were equal to the average.
func withMax(avg, max map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(avg))
	for k, v := range avg {
		out[k] = v
	}
	for k, v := range max {
		out[k] = v
	}
	return out
}

// State is the small live file read by `azor` (and later by a hook).
type State struct {
	T        int64              `json:"t"`
	Values   map[string]float64 `json:"values"`
	Alerts   []string           `json:"alerts"`
	CPU      []proc             `json:"top_cpu"`
	Mem      []proc             `json:"top_mem"`
	Watch    []proc             `json:"watch,omitempty"`
	Services []proc             `json:"services,omitempty"`
	FS       map[string]float64 `json:"fs,omitempty"`
	Facts    map[string]string  `json:"facts,omitempty"`
	Failed   []string           `json:"failed,omitempty"`
}

func runDaemon() error {
	syscall.Umask(0o077)
	debug.SetGCPercent(25) // little garbage; memory matters more than CPU here
	debug.SetMemoryLimit(12 << 20)
	cfg, warn := loadConfig()
	for _, w := range warn {
		fmt.Fprintln(os.Stderr, "azor: config:", w)
	}
	gapAfter := cfg.gapAfter()
	dir := dataDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(stateFile()), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return fmt.Errorf("another azor daemon is running")
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	store := func(r Record) {
		if err := appendRecord(dir, r); err != nil {
			fmt.Fprintln(os.Stderr, "azor: write:", err)
		}
	}
	now := time.Now()
	prune(dir, now, cfg.Retention)
	var last int64
	load(dir, now.AddDate(0, 0, -cfg.Retention), now, func(r Record) {
		if r.Type == "sample" {
			last = r.T
		}
	})
	if last > 0 && now.Unix()-last > int64(gapAfter.Seconds()) {
		store(Record{T: now.Unix(), Type: "event", Kind: "gap", Secs: now.Unix() - last})
	}
	store(Record{T: now.Unix(), Type: "event", Kind: "start"})

	s, ps, a := newSensors(cfg, ""), &procScan{}, newAgg()
	al := alerts{makeRules(cfg, s.crit), map[string]*alert{}}
	st, pac, svc, conf := State{}, newPacTail("/var/log/pacman.log"), &svcScan{}, newConfigScan()
	if cfg.on("processes") {
		_, _, st.Watch = ps.top(0, cfg.TopN, cfg.WatchProcs)
	}
	lastFlush, lastProc, lastTick, day := now, now, now, now.YearDay()
	var svcNames []string
	for _, p := range svc.scan(userCgroup(), 0) {
		svcNames = append(svcNames, p.Name)
	}
	// slow watches settings, services and declared files for changes.
	seen := map[string]map[string]string{}
	change := func(kind string, cur map[string]string, t time.Time) {
		old, known := seen[kind]
		if cur == nil {
			return
		}
		seen[kind] = cur
		if !known {
			return
		}
		for _, k := range diff(old, cur) {
			e := Record{T: t.Unix(), Type: "event", Kind: kind, Key: short(k)}
			if kind != "file" {
				e.Old, e.New = old[k], cur[k]
			}
			store(e)
		}
	}
	slow := func(t time.Time) {
		if cfg.on("facts") {
			st.Facts = s.facts()
			change("fact", st.Facts, t)
		}
		if cfg.on("services") {
			st.Failed = nil
			f := failedUnits()
			for k := range f {
				st.Failed = append(st.Failed, k)
			}
			sort.Strings(st.Failed)
			change("unit", f, t)
		}
		if cfg.on("files") {
			change("file", fileMarks(cfg.WatchFiles), t)
			if time.Since(conf.found) > 10*time.Minute {
				conf.discover(append(running(st), svcNames...))
				debug.FreeOSMemory() // the package list is large and short-lived
			}
			for _, e := range conf.changes() {
				e.T = t.Unix()
				store(e)
			}
		}
		if cfg.on("packages") {
			if sum, names := pac.next(); sum != "" {
				store(Record{T: t.Unix(), Type: "event", Kind: "package", Key: sum, New: names})
				conf.found = time.Time{} // programs changed
			}
		}
	}
	// Facts survive restarts, so a new kernel or driver setting is noticed.
	seen["fact"] = lastFacts(dir, now, cfg.Retention)
	if seen["fact"]["facts_version"] != factsVersion {
		delete(seen, "fact")
	}
	slow(now)
	store(Record{T: now.Unix(), Type: "facts", Facts: st.Facts})
	tk := time.NewTicker(cfg.Sample)
	defer tk.Stop()
	for {
		select {
		case <-stop:
			os.Remove(stateFile())
			return nil
		case <-tk.C:
		}
		// Round(0) drops the monotonic clock, which stops during suspend.
		now = time.Now().Round(0)
		if d := now.Sub(lastTick.Round(0)); d > gapAfter {
			store(Record{T: now.Unix(), Type: "event", Kind: "gap", Secs: int64(d.Seconds())})
			a, lastFlush, lastProc = newAgg(), now, now
			s.discover()
			s.cpuBusy()
			if cfg.on("processes") {
				_, _, st.Watch = ps.top(0, cfg.TopN, cfg.WatchProcs)
			}
			lastTick = now
			continue
		}
		lastTick = now
		a.add(s.read())
		if now.Sub(lastFlush.Round(0)) < cfg.Interval-cfg.Sample/2 {
			continue
		}
		lastFlush = now
		avg, max := a.result()
		a = newAgg()
		store(Record{T: now.Unix(), Type: "sample", Avg: avg, Max: max})
		for _, e := range al.update(now.Unix(), avg, withMax(avg, max)) {
			store(e)
		}
		if d := now.Sub(lastProc.Round(0)); d >= cfg.ProcInterval-cfg.Sample {
			lastProc = now
			if cfg.on("processes") {
				st.CPU, st.Mem, st.Watch = ps.top(d.Seconds(), cfg.TopN, cfg.WatchProcs)
				all := svc.scan(userCgroup(), d.Seconds())
				svcNames = nil
				for _, p := range all {
					svcNames = append(svcNames, p.Name)
				}
				st.Services = all[:min(len(all), cfg.TopN)]
				store(Record{T: now.Unix(), Type: "procs", CPU: st.CPU, Mem: st.Mem, Watch: st.Watch, Services: st.Services})
			}
			if cfg.on("disks") {
				if fs := filesystems(readStr("/proc/self/mounts"), fsUsed); len(fs) > 0 {
					for k, v := range fs {
						fs[k] = round1(v)
					}
					st.FS = fs
					store(Record{T: now.Unix(), Type: "fs", Avg: fs})
				}
			}
			slow(now)
		}
		st.T, st.Values, st.Alerts = now.Unix(), avg, nil
		for _, r := range al.rules {
			if x := al.m[r.Key]; x != nil && x.active {
				st.Alerts = append(st.Alerts, r.Key)
			}
		}
		st.clean()
		if err := writeAtomic(stateFile(), st); err != nil {
			fmt.Fprintln(os.Stderr, "azor: state:", err)
		}
		if now.YearDay() != day {
			day = now.YearDay()
			prune(dir, now, cfg.Retention)
		}
	}
}

// running names everything currently observed, to match configuration.
func running(st State) (out []string) {
	for _, l := range [][]proc{st.CPU, st.Mem, st.Watch, st.Services} {
		for _, p := range l {
			out = append(out, p.Name)
		}
	}
	for _, k := range []string{"desktop", "wayland_server", "x11_server"} {
		out = append(out, strings.FieldsFunc(st.Facts[k], func(r rune) bool { return r == ' ' || r == ':' })...)
	}
	return
}
