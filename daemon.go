package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
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
		off := on - span
		if on > 0 && off <= 0 {
			off = on / 2 // a low user threshold must still be able to clear
		}
		out = append(out, rule{key, on, off, hold})
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
	if _, own := c.Alerts["disk_temp"]; own {
		temp("disk_temp", 75) // the user's absolute limit for the hottest disk
	} else {
		add("disk_over", 0, 7, 2) // each disk against its own limit
	}
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
	System   []proc             `json:"system,omitempty"`
	FS       map[string]float64 `json:"fs,omitempty"`
	Facts    map[string]string  `json:"facts,omitempty"`
	Failed   []string           `json:"failed,omitempty"`
	// Sources that could not be read; what was known before is kept.
	Unknown []string `json:"unknown,omitempty"`
	// Set while the history cannot be written.
	WriteError string   `json:"write_error,omitempty"`
	WriteSince int64    `json:"write_since,omitempty"`
	Timers     []timer  `json:"timers,omitempty"`
	Problems   []string `json:"problems,omitempty"`
	Desktop    []proc   `json:"desktop,omitempty"`
}

// factModules names the modules that decide which facts exist.
func factModules(c Config) string {
	var on []string
	for _, m := range []string{"facts", "filesystem", "desktop", "gpu"} {
		if c.on(m) {
			on = append(on, m)
		}
	}
	return strings.Join(on, ",")
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

	var st State
	var lastSaved int64
	// The time from which writing fails is also kept outside the history,
	// so that a restart does not forget it.
	// Twice: systemd may remove the runtime directory, and the data
	// directory may be the one that cannot be written.
	markers := []string{filepath.Join(filepath.Dir(stateFile()), "write_failed"), filepath.Join(dir, ".write_failed")}
	for _, m := range markers {
		var since int64
		b, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		if _, err := fmt.Sscan(string(b), &since); err == nil && since > 0 && (st.WriteSince == 0 || since < st.WriteSince) {
			st.WriteSince, st.WriteError = since, "writing failed before the last start"
		}
	}
	store := func(r Record) {
		if err := appendRecord(dir, r); err != nil {
			if st.WriteSince == 0 {
				st.WriteSince = r.T
				fmt.Fprintln(os.Stderr, "azor: write:", err)
				for _, m := range markers {
					os.WriteFile(m, []byte(fmt.Sprint(r.T)), 0o600)
				}
			}
			st.WriteError = err.Error()
			return
		}
		if st.WriteSince != 0 { // the time in between is missing from the history
			from := st.WriteSince - 1
			if lastSaved > from {
				from = lastSaved
			}
			if appendRecord(dir, Record{T: r.T, Type: "event", Kind: "lost", Secs: max(1, r.T-from)}) == nil {
				st.WriteSince, st.WriteError = 0, ""
				for _, m := range markers {
					os.Remove(m)
				}
			}
		}
		lastSaved = r.T
	}
	now := time.Now()
	keep := func(now time.Time, daily bool) {
		if daily {
			if err := compact(dir, now, cfg.Retention, cfg.SnapKeep, cfg.Interval.Seconds()); err != nil {
				fmt.Fprintln(os.Stderr, "azor: long-term history:", err)
				return
			}
			prune(dir, now, cfg.Retention) // a day file is removed only once it is summed up
		}
		es, err := fitSpace(dir, now, int64(cfg.MaxDisk)*1e6, cfg.Interval.Seconds())
		if err != nil {
			fmt.Fprintln(os.Stderr, "azor: disk limit:", err)
		}
		for _, e := range es {
			store(e)
		}
	}
	keep(now, true)
	last := lastSample(dir, now)
	lastSaved = last
	prev, known := loadSession(dir)
	cur := runInfo{Boot: bootID(), Start: now.Unix(), Last: now.Unix()}
	for _, e := range startEvents(prev, known, cur.Boot, now.Unix(), last, int64(gapAfter.Seconds())) {
		store(e)
	}
	saveSession(dir, cur)
	sleep := newSleepWatch()
	snaps := newSnapshots(cfg)

	s, ps, a := newSensors(cfg, ""), &procScan{}, newAgg()
	al := alerts{makeRules(cfg, s.crit), map[string]*alert{}}
	pac, svc, sys, conf := newPacTail("/var/log/pacman.log"), &svcScan{}, &svcScan{}, newConfigScan()
	if cfg.on("processes") {
		_, _, st.Watch = ps.top(0, cfg.TopN, cfg.WatchProcs)
	}
	lastFlush, lastProc, lastTick, day := now, now, now, now.YearDay()
	spaceAt := now
	var svcNames []string
	for _, p := range svc.scan(userCgroup(), 0) {
		svcNames = append(svcNames, p.Name)
	}
	sys.scan(systemCgroup, 0)
	// slow watches settings, services and declared files for changes.
	seen := map[string]map[string]string{}
	runs, lastRun, logged, runDur := map[string]run{}, map[string]int64{}, map[string]int64{}, map[string]int64{}
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
			if kind == "timer" {
				e.Val, e.Secs = float64(lastRun[k]), runDur[k] // which run the result belongs to
			}
			if kind != "file" {
				e.Old, e.New = old[k], cur[k]
			}
			store(e)
		}
	}
	var timersAt time.Time
	var unknownSave []string
	units := map[string]map[string]string{}
	var unknownTimers, unknownUnits, unknownWatched []string
	factsRebased := false
	desktopAt := now
	var hl *health
	var dt *desktop
	if cfg.on("desktop") {
		dt = newDesktop()
	}
	if cfg.on("health") {
		hl = newHealth("")
	}
	slow := func(t time.Time) {
		if hl != nil {
			var es []Record
			es, st.Problems = hl.run(t)
			for _, e := range es {
				store(e)
			}
		}
		if cfg.on("timers") && time.Since(timersAt) >= 5*time.Minute {
			timersAt = time.Now()
			ts, ok, unk := readTimers(cfg.IgnoreTimers)
			unknownTimers = unk
			if ok || len(st.Timers) > 0 {
				ts = mergeTimers(st.Timers, ts, unk, runs)
				st.Timers = ts
				// A result is kept until the next known one, also over a reboot.
				cur := timerStates(ts)
				for k, v := range seen["timer"] {
					if _, ok := cur[k]; !ok {
						cur[k] = v
					}
				}
				for _, x := range ts { // the run each result belongs to
					if s, ok := timerStates([]timer{x})[timerKey(x)]; ok {
						runs[timerKey(x)] = run{s, x.Last}
						lastRun[timerKey(x)], runDur[timerKey(x)] = x.Last, x.Dur
					}
				}
				before := seen["timer"]
				change("timer", cur, t)
				for k, r := range runs { // every new run is kept, also with the same result
					if logged[k] != r.Last && before[k] == r.State {
						store(Record{T: t.Unix(), Type: "event", Kind: "timer", Key: k, Old: r.State, New: r.State, Val: float64(r.Last), Secs: runDur[k]})
					}
					logged[k] = r.Last
				}
				unknownSave = nil
				if err := saveRuns(dir, runs); err != nil {
					unknownSave = []string{"timer results not saved (" + err.Error() + ")"}
				}
			}
		}
		if cfg.on("devices") {
			change("usb", usbDevices(""), t)
			change("link", netLinks(""), t)
		}
		if u, complete := watchedUnits(cfg.WatchUnits); u != nil {
			if !complete { // a unit that could not be read keeps its last state
				for k, v := range seen["watched"] {
					if _, ok := u[k]; !ok {
						u[k] = v
					}
				}
			}
			change("watched", u, t)
			unknownWatched = nil
			if !complete {
				unknownWatched = []string{"watched units"}
			}
		}
		if cfg.on("facts") || dt != nil || cfg.on("filesystem") {
			if cfg.on("facts") {
				st.Facts = s.facts()
			} else {
				st.Facts = map[string]string{}
				if cfg.on("filesystem") {
					s.fsFacts(func(k, v string) {
						if v != "" {
							st.Facts[k] = v
						}
					})
				}
			}
			st.Facts["facts_modules"] = factModules(cfg)
			if dt != nil {
				dt.facts(t, func(k, v string) {
					if v != "" {
						st.Facts[cleanTo(k, 80)] = v
					}
				})
			}
			if !cfg.on("facts") && !factsRebased { // only what is still collected is compared
				factsRebased = true
				part := map[string]string{}
				for k := range st.Facts {
					if v, ok := seen["fact"][k]; ok {
						part[k] = v
					}
				}
				if _, ok := seen["fact"]; ok {
					seen["fact"] = part
				}
				// nor is what the saved baseline never had: no baseline is kept here
				cur := map[string]string{}
				for k, v := range st.Facts {
					if _, ok := part[k]; ok || seen["fact"] == nil {
						cur[k] = v
					}
				}
				change("fact", cur, t)
			} else {
				change("fact", st.Facts, t)
			}
			if cfg.on("facts") {
				saveFacts(dir, st.Facts)
			}
		}
		if dt != nil {
			for _, e := range dt.changes(t.Unix()) {
				store(e)
			}
			st.Desktop = dt.scan(t.Sub(desktopAt).Seconds())
			desktopAt = t
			if len(st.Desktop) > 0 {
				store(Record{T: t.Unix(), Type: "procs", MemDef: 3, Desktop: st.Desktop})
			}
		}
		if cfg.on("services") {
			var f map[string]string
			f, unknownUnits = mergeFailed(units, failedUnits())
			st.Failed = sortedKeys(f)
			change("unit", f, t)
		}
		st.Unknown = append(append(append(append([]string{}, unknownUnits...), unknownTimers...), unknownWatched...), unknownSave...)
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
				if dt != nil {
					dt.versions = nil
				}
			}
		}
	}
	// Facts survive restarts, so a new kernel or driver setting is noticed.
	if cfg.on("timers") {
		runs = knownRuns(dir, now, cfg.Retention)
		seen["timer"] = map[string]string{}
		for k, r := range runs {
			seen["timer"][k] = r.State
			logged[k] = r.Last
		}
	}
	seen["fact"] = lastFacts(dir, now, cfg.Retention)
	// Another program version or another set of modules collects other facts:
	// what is new or gone then is not a change of the computer.
	if seen["fact"]["facts_version"] != factsVersion || seen["fact"]["facts_modules"] != factModules(cfg) {
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
			cur.Last, cur.Stopped, cur.Shutdown = time.Now().Unix(), true, shuttingDown()
			kind := "stop"
			if cur.Shutdown {
				kind = "shutdown"
			}
			store(Record{T: cur.Last, Type: "event", Kind: kind})
			saveSession(dir, cur)
			return nil
		case <-tk.C:
		}
		// Round(0) drops the monotonic clock, which stops during suspend.
		now = time.Now().Round(0)
		slept := sleep.slept()
		if slept > 0 { // the boot clock went on while the monotonic one stood still
			store(Record{T: now.Unix(), Type: "event", Kind: "suspend", Secs: int64(slept.Round(time.Second).Seconds())})
		}
		if d := now.Sub(lastTick.Round(0)); d > gapAfter || slept > 0 {
			if slept == 0 || d-slept > gapAfter { // a pause the suspend does not explain
				store(Record{T: now.Unix(), Type: "event", Kind: "gap", Secs: int64((d - slept).Seconds())})
			}
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
		span := now.Sub(lastFlush)
		lastFlush = now
		avg, max := a.result()
		a = newAgg()
		store(Record{T: now.Unix(), Type: "sample", Avg: avg, Max: max, Secs: int64(span.Round(time.Second).Seconds())})
		var fired []string
		for _, e := range al.update(now.Unix(), avg, withMax(avg, max)) {
			store(e)
			if e.Kind == "alert" {
				fired = append(fired, e.Key)
			}
		}
		if cfg.on("snapshots") {
			if why, ok := snaps.due(now, fired); ok {
				st.Values = avg
				store(takeSnapshot(now, why, st, cfg.on("processes"), cfg.SnapTop))
			}
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
				st.Services = notable(all, cfg.TopN)
				if cfg.on("services") {
					st.System = notable(sys.scan(systemCgroup, d.Seconds()), 3)
				}
				store(Record{T: now.Unix(), Type: "procs", MemDef: 3, CPU: st.CPU, Mem: st.Mem, Watch: st.Watch, Services: st.Services, System: st.System})
			}
			if cfg.on("disks") || cfg.on("filesystem") {
				if fs := filesystems(readStr("/proc/self/mounts"), fsUsed); len(fs) > 0 {
					for k, v := range fs {
						fs[k] = round1(v)
					}
					st.FS = fs
					store(Record{T: now.Unix(), Type: "fs", Avg: fs})
				}
			}
			slow(now)
			cur.Last = now.Unix() // so that the next start knows how far this run got
			saveSession(dir, cur)
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
			keep(now, true)
			spaceAt = now
		} else if now.Sub(spaceAt) >= time.Hour { // today's file grows too
			keep(now, false)
			spaceAt = now
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
	for _, k := range []string{"desktop", "display_server"} {
		out = append(out, strings.FieldsFunc(st.Facts[k], func(r rune) bool { return r == ' ' || r == ':' })...)
	}
	return
}
