package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type metric struct{ key, group, label, unit string }

var metrics = []metric{
	{"cpu", "CPU", "load", "%"}, {"cpu_temp", "CPU", "temperature", "C"}, {"cpu_mhz", "CPU", "clock average", "MHz"}, {"cpu_mhz_top", "CPU", "clock fastest core", "MHz"}, {"load1", "CPU", "load average 1m", ""},
	{"mem", "Memory", "RAM used", "%"}, {"swap", "Memory", "swap used", "%"},
	{"gpu", "GPU", "load", "%"}, {"gpu_video", "GPU", "video engine", "%"}, {"gpu_temp", "GPU", "temperature", "C"}, {"gpu_junction", "GPU", "junction", "C"}, {"gpu_mem_temp", "GPU", "memory temp", "C"},
	{"gpu_power", "GPU", "power", "W"}, {"gpu_clock", "GPU", "core clock", "MHz"}, {"gpu_mem_clock", "GPU", "memory clock", "MHz"}, {"gpu_vram_mb", "GPU", "VRAM used", "MB"}, {"gpu_fan", "GPU", "fan", "RPM"}, {"gpu_fan_pct", "GPU", "fan", "%"},
	{"disk_temp", "Disks", "hottest", "C"}, {"net_down", "Network", "download", "MB/s"}, {"net_up", "Network", "upload", "MB/s"},
	{"system_fan", "System", "fastest fan", "RPM"}, {"battery", "System", "battery", "%"}, {"battery_power", "System", "battery power", "W"},
	{"psi_cpu", "Pressure", "CPU stall", "%"}, {"psi_memory", "Pressure", "memory stall", "%"}, {"psi_io", "Pressure", "I/O stall", "%"},
}

// describe also covers per-disk and per-filesystem metrics.
func describe(key string) metric {
	for _, m := range metrics {
		if m.key == key {
			return m
		}
	}
	if r, ok := strings.CutPrefix(key, "disk."); ok {
		if i := strings.LastIndexByte(r, '.'); i > 0 {
			u := map[string]string{"temp": "C", "read": "MB/s", "write": "MB/s", "busy": "%"}[r[i+1:]]
			return metric{key, "Disks", r[:i] + " " + r[i+1:], u}
		}
	}
	if r, ok := strings.CutPrefix(key, "fs."); ok {
		return metric{key, "Filesystems", strings.TrimSuffix(r, ".used"), "% used"}
	}
	return metric{key, "Other", key, ""}
}

// ordered returns known metrics first, then dynamic ones by name.
func ordered[T any](vals map[string]T) (out []metric) {
	done := map[string]bool{}
	for _, m := range metrics {
		if _, ok := vals[m.key]; ok {
			out = append(out, m)
			done[m.key] = true
		}
	}
	var rest []string
	for k := range vals {
		if !done[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		out = append(out, describe(k))
	}
	groups := []string{"CPU", "Memory", "GPU", "Disks", "Filesystems", "Network", "System", "Pressure", "Other"}
	pos := map[string]int{}
	for i, g := range groups {
		pos[g] = i
	}
	sort.SliceStable(out, func(a, b int) bool { return pos[out[a].group] < pos[out[b].group] })
	return
}

func label(key string) (string, string) {
	m := describe(key)
	return m.group + " " + m.label, m.unit
}

func procList(ps []proc) string {
	var s []string
	for _, p := range ps {
		if p.CPU > 0 {
			s = append(s, fmt.Sprintf("%s %.0f%%", p.Name, p.CPU))
		} else {
			s = append(s, fmt.Sprintf("%s %.0f MB", p.Name, p.MB))
		}
	}
	if len(s) == 0 {
		return "-"
	}
	return strings.Join(s, ", ")
}

func cmdNow(w io.Writer) error {
	b, err := os.ReadFile(stateFile())
	if err != nil {
		return fmt.Errorf("no live state; is the daemon running? (systemctl --user status azor)")
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	s.clean()
	age := time.Since(time.Unix(s.T, 0)).Round(time.Second)
	fmt.Fprintf(w, "State at %s (%s ago)\n", time.Unix(s.T, 0).Format("15:04:05"), age)
	cfg, _ := loadConfig()
	if age > cfg.gapAfter() {
		fmt.Fprintln(w, "WARNING: state is stale, the daemon may have stopped")
	}
	all := map[string]float64{}
	for k, v := range s.Values {
		all[k] = v
	}
	for k, v := range s.FS {
		all[k] = v
	}
	group := ""
	for _, m := range ordered(all) {
		if m.group != group {
			group = m.group
			fmt.Fprintln(w, group)
		}
		fmt.Fprintf(w, "  %-20s %9.1f %s\n", m.label, all[m.key], m.unit)
	}
	fmt.Fprintln(w, "Processes (1 thread = 100%)")
	fmt.Fprintf(w, "  CPU: %s\n  RAM: %s\n", procList(s.CPU), procList(s.Mem))
	if len(s.Services) > 0 {
		fmt.Fprintln(w, "User services")
		for _, p := range s.Services {
			fmt.Fprintf(w, "  %-28s CPU %3.0f%%  RAM %5.0f MB\n", p.Name, p.CPU, p.MB)
		}
	}
	for _, p := range s.Watch {
		fmt.Fprintf(w, "  watched %s: CPU %.0f%%, RAM %.0f MB\n", p.Name, p.CPU, p.MB)
	}
	if len(s.Facts) > 0 {
		fmt.Fprintln(w, "Hardware and settings")
		var ks []string
		for k := range s.Facts {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			if k == "facts_version" {
				continue
			}
			fmt.Fprintf(w, "  %-20s %s\n", k, s.Facts[k])
		}
	}
	if len(s.Failed) > 0 {
		fmt.Fprintln(w, "FAILED SERVICES:", strings.Join(s.Failed, ", "))
	}
	if len(s.Alerts) > 0 {
		fmt.Fprintln(w, "ACTIVE ALERTS:", strings.Join(s.Alerts, ", "))
	} else {
		fmt.Fprintln(w, "No active alerts.")
	}
	return nil
}

// parseTime accepts HH:MM (latest past occurrence), a full date and time, or now.
func parseTime(s string, now time.Time) (time.Time, error) {
	if s == "now" {
		return now, nil
	}
	if t, err := time.ParseInLocation("15:04", s, time.Local); err == nil {
		r := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, time.Local)
		if r.After(now) {
			r = r.AddDate(0, 0, -1)
		}
		return r, nil
	}
	for _, l := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("bad time %q", s)
}

func parseRange(a, b string, now time.Time) (from, to time.Time, err error) {
	if from, err = parseTime(a, now); err != nil {
		return
	}
	if to, err = parseTime(b, now); err != nil {
		return
	}
	if to.Before(from) && len(a) == 5 { // 22:00 01:00 crosses midnight
		from = from.AddDate(0, 0, -1)
	}
	if !to.After(from) {
		err = fmt.Errorf("the range is empty")
	}
	return
}

func since(args []string) (from, to time.Time, err error) {
	d := 24 * time.Hour
	if len(args) > 0 {
		if d, err = time.ParseDuration(args[0]); err != nil || d <= 0 {
			return from, to, fmt.Errorf("bad duration %q", args[0])
		}
	}
	to = time.Now()
	return to.Add(-d), to, nil
}

type peak struct {
	v float64
	t int64
}

type summary struct {
	n     int
	first int64
	last  int64
	sum   map[string]float64
	cnt   map[string]int
	peaks map[string]peak
}

func newSummary() *summary {
	return &summary{sum: map[string]float64{}, cnt: map[string]int{}, peaks: map[string]peak{}}
}

func (s *summary) add(r Record) {
	if s.n == 0 {
		s.first = r.T
	}
	s.n++
	s.last = r.T
	for k, v := range withMax(r.Avg, r.Max) {
		if p, ok := s.peaks[k]; !ok || v > p.v {
			s.peaks[k] = peak{v, r.T}
		}
	}
	for k, v := range r.Avg {
		s.sum[k] += v
		s.cnt[k]++
	}
}

func (s *summary) print(w io.Writer) {
	fmt.Fprintf(w, "  %-28s %9s %9s  %s\n", "metric", "average", "peak", "peak at")
	for _, m := range ordered(s.cnt) {
		p := s.peaks[m.key]
		l, u := label(m.key)
		if strings.HasPrefix(m.key, "disk.") && p.v < 0.1 {
			continue // an idle disk adds nothing
		}
		fmt.Fprintf(w, "  %-28s %9.1f %9.1f  %s  %s\n", l, s.sum[m.key]/float64(s.cnt[m.key]), p.v, time.Unix(p.t, 0).Format("01-02 15:04:05"), u)
	}
}

func eventLine(r Record) string {
	at := time.Unix(r.T, 0).Format("01-02 15:04:05")
	l, u := label(r.Key)
	switch r.Kind {
	case "alert":
		return fmt.Sprintf("%s  ALERT  %s at %.1f %s", at, l, r.Val, u)
	case "clear":
		return fmt.Sprintf("%s  clear  %s after %s, peak %.1f %s", at, l, time.Duration(r.Secs)*time.Second, r.Val, u)
	case "gap":
		return fmt.Sprintf("%s  GAP    no data for the previous %s (suspended, off or daemon stopped)", at, time.Duration(r.Secs)*time.Second)
	case "fact":
		return fmt.Sprintf("%s  change %s: %q -> %q", at, r.Key, r.Old, r.New)
	case "unit":
		if r.New == "" {
			return fmt.Sprintf("%s  ok     %s recovered", at, r.Key)
		}
		return fmt.Sprintf("%s  FAILED %s", at, r.Key)
	case "file":
		return fmt.Sprintf("%s  file   %s changed", at, r.Key)
	case "config":
		return fmt.Sprintf("%s  config %s: %s", at, r.Key, r.New)
	case "package":
		return fmt.Sprintf("%s  pkg    %s: %s", at, r.Key, r.New)
	case "start":
		return at + "  start  daemon started"
	}
	return at + "  " + r.Kind
}

func cmdRange(w io.Writer, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: azor range FROM TO")
	}
	from, to, err := parseRange(args[0], args[1], time.Now())
	if err != nil {
		return err
	}
	cfg, _ := loadConfig()
	const rows = 12
	step := to.Sub(from) / rows
	all, parts := newSummary(), make([]*summary, rows)
	var events []Record
	svcs := map[string][2]float64{}
	cpu, mem, watch, fs := map[string]float64{}, map[string]float64{}, map[string][2]float64{}, map[string][2]float64{}
	err = load(dataDir(), from, to, func(r Record) {
		switch r.Type {
		case "sample":
			all.add(r)
			i := min(int(time.Unix(r.T, 0).Sub(from)/step), rows-1)
			if parts[i] == nil {
				parts[i] = newSummary()
			}
			parts[i].add(r)
		case "event":
			events = append(events, r)
		case "procs":
			for _, p := range r.CPU {
				cpu[p.Name] = max(cpu[p.Name], p.CPU)
			}
			for _, p := range r.Mem {
				mem[p.Name] = max(mem[p.Name], p.MB)
			}
			for _, p := range r.Services {
				old, seen := svcs[p.Name]
				if !seen {
					old[0] = p.MB
				}
				svcs[p.Name] = [2]float64{old[0], p.MB}
			}
			for _, p := range r.Watch {
				old, seen := watch[p.Name]
				if !seen {
					old[0] = p.MB
				}
				watch[p.Name] = [2]float64{old[0], p.MB}
			}
		case "fs":
			for k, v := range r.Avg {
				old, seen := fs[k]
				if !seen {
					old[0] = v
				}
				fs[k] = [2]float64{old[0], v}
			}
		}
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Range %s - %s\n", from.Format("2006-01-02 15:04"), to.Format("2006-01-02 15:04"))
	if all.n == 0 {
		fmt.Fprintln(w, "No samples in this range.")
	} else {
		cover := float64(all.n) * cfg.Interval.Seconds() / to.Sub(from).Seconds() * 100
		fmt.Fprintf(w, "%d samples, about %.0f%% of the range covered\n", all.n, min(cover, 100))
		all.print(w)
		fmt.Fprintln(w, "Timeline (averages, peak in brackets):")
		for i, p := range parts {
			at := from.Add(time.Duration(i) * step).Format("01-02 15:04")
			if p == nil {
				fmt.Fprintf(w, "  %s  no data\n", at)
				continue
			}
			var cols []string
			for _, c := range []struct{ key, name, unit string }{{"cpu", "CPU", "%"}, {"cpu_temp", "", "C"}, {"gpu", "GPU", "%"},
				{"gpu_temp", "", "C"}, {"gpu_power", "", "W"}, {"gpu_fan", "fan", "RPM"}, {"gpu_fan_pct", "fan", "%"}, {"mem", "RAM", "%"}} {
				if n := p.cnt[c.key]; n > 0 {
					cols = append(cols, strings.TrimSpace(fmt.Sprintf("%s %.0f%s (%.0f)", c.name, p.sum[c.key]/float64(n), c.unit, p.peaks[c.key].v)))
				}
			}
			fmt.Fprintf(w, "  %s  %s\n", at, strings.Join(cols, "  "))
		}
		fmt.Fprintln(w, "Busiest processes (peak, 1 thread = 100%):", names(cpu, "%"))
		fmt.Fprintln(w, "Largest processes (peak):", names(mem, " MB"))
	}
	for _, n := range sortedKeys(svcs) {
		fmt.Fprintf(w, "Service %s: RAM %.0f -> %.0f MB\n", n, svcs[n][0], svcs[n][1])
	}
	for _, n := range sortedKeys(watch) {
		fmt.Fprintf(w, "Watched %s: RAM %.0f -> %.0f MB\n", n, watch[n][0], watch[n][1])
	}
	for _, k := range sortedKeys(fs) {
		fmt.Fprintf(w, "Filesystem %s: %.1f -> %.1f %% used\n", describe(k).label, fs[k][0], fs[k][1])
	}
	fmt.Fprintln(w, "Events:")
	for _, e := range events {
		fmt.Fprintln(w, " ", eventLine(e))
	}
	if len(events) == 0 {
		fmt.Fprintln(w, "  none")
	}
	if cfg.on("journal") {
		journal(w, from, to)
	}
	if cfg.on("packages") {
		pacman(w, "/var/log/pacman.log", from, to)
	}
	return nil
}

func sortedKeys[T any](m map[string]T) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func names(m map[string]float64, unit string) string {
	var xs []kv
	for k, v := range m {
		xs = append(xs, kv{k, v})
	}
	sort.Slice(xs, func(a, b int) bool { return xs[a].v > xs[b].v })
	var out []string
	for _, x := range xs[:min(len(xs), 6)] {
		out = append(out, fmt.Sprintf("%s %.0f%s", x.k, x.v, unit))
	}
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, ", ")
}

var unitName = regexp.MustCompile(`^[A-Za-z0-9@._:\\-]{1,80}$`)

// journal prints error counts per unit. Messages are never read into the
// output, because they may contain private data.
func journal(w io.Writer, from, to time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "journalctl", "-q", "--no-pager", "-p", "err", "-o", "json",
		"--output-fields=_SYSTEMD_UNIT,_SYSTEMD_USER_UNIT,SYSLOG_IDENTIFIER",
		"--since", from.Format("2006-01-02 15:04:05"), "--until", to.Format("2006-01-02 15:04:05"))
	out, err := c.StdoutPipe()
	if err != nil || c.Start() != nil {
		fmt.Fprintln(w, "Journal errors: unavailable")
		return
	}
	n := map[string]float64{}
	d := json.NewDecoder(bufio.NewReader(out))
	for {
		var e map[string]any
		if d.Decode(&e) != nil {
			break
		}
		name := "other"
		for _, k := range []string{"_SYSTEMD_USER_UNIT", "_SYSTEMD_UNIT", "SYSLOG_IDENTIFIER"} {
			if s, ok := e[k].(string); ok && unitName.MatchString(s) && s != "init.scope" {
				name = s
				break
			}
		}
		n[name]++
	}
	if c.Wait() != nil && len(n) == 0 {
		fmt.Fprintln(w, "Journal errors: unavailable")
		return
	}
	fmt.Fprintln(w, "Journal errors (count per unit):", names(n, "x"))
	if o := oldestJournal(); !o.IsZero() && from.Before(o) {
		fmt.Fprintln(w, "  note: the journal only reaches back to", o.Format("2006-01-02 15:04"))
	}
}

// oldestJournal uses the list of boots, which contains no log messages.
func oldestJournal() time.Time {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "journalctl", "-q", "--no-pager", "--list-boots", "-o", "json").Output()
	if err != nil {
		return time.Time{}
	}
	return oldestBoot(b)
}

func oldestBoot(b []byte) (t time.Time) {
	var boots []struct {
		First int64 `json:"first_entry"`
	}
	if json.Unmarshal(b, &boots) != nil {
		return
	}
	for _, x := range boots {
		if at := time.UnixMicro(x.First); x.First > 0 && (t.IsZero() || at.Before(t)) {
			t = at
		}
	}
	return
}

var pacLine = regexp.MustCompile(`^\[([^\]]+)\] \[ALPM\] (installed|upgraded|downgraded|removed) (\S+)`)

func pacman(w io.Writer, path string, from, to time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		m := pacLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		t, err := time.Parse("2006-01-02T15:04:05-0700", m[1])
		if err == nil && !t.Before(from) && !t.After(to) {
			out = append(out, m[2]+" "+clean(m[3]))
		}
	}
	more := ""
	if len(out) > 15 {
		more = fmt.Sprintf(" (+%d more)", len(out)-15)
		out = out[:15]
	}
	if len(out) == 0 {
		out = []string{"none"}
	}
	fmt.Fprintln(w, "Package changes:", strings.Join(out, ", ")+more)
}

func cmdPeaks(w io.Writer, args []string) error {
	from, to, err := since(args)
	if err != nil {
		return err
	}
	s := newSummary()
	if err := load(dataDir(), from, to, func(r Record) {
		if r.Type == "sample" {
			s.add(r)
		}
	}); err != nil {
		return err
	}
	fmt.Fprintf(w, "Peaks since %s (%d samples)\n", from.Format("2006-01-02 15:04"), s.n)
	s.print(w)
	return nil
}

func cmdEvents(w io.Writer, args []string) error {
	from, to, err := since(args)
	if err != nil {
		return err
	}
	n := 0
	err = load(dataDir(), from, to, func(r Record) {
		if r.Type == "event" {
			n++
			fmt.Fprintln(w, eventLine(r))
		}
	})
	if n == 0 && err == nil {
		fmt.Fprintln(w, "No events.")
	}
	return err
}

func cmdStatus(w io.Writer) error {
	cfg, _ := loadConfig()
	fs := dayFiles(dataDir())
	var size int64
	for _, n := range fs {
		if i, err := os.Stat(filepath.Join(dataDir(), n)); err == nil {
			size += i.Size()
		}
	}
	live := "not running or no data yet"
	if i, err := os.Stat(stateFile()); err == nil {
		age := time.Since(i.ModTime()).Round(time.Second)
		live = fmt.Sprintf("last update %s ago", age)
		if age > cfg.gapAfter() {
			live += " (STALE)"
		}
	}
	fmt.Fprintf(w, "daemon: %s\ndata: %s, %d day files, %.1f MB, retention %d days\n", live, dataDir(), len(fs), float64(size)/1e6, cfg.Retention)
	return nil
}

// cmdHardware shows what was detected: drivers and limits, no serial numbers.
func cmdHardware(w io.Writer) error {
	cfg, _ := loadConfig()
	s := newSensors(cfg, "")
	if s.nvidia != nil {
		s.nvidia.poll()
	}
	s.read()
	time.Sleep(500 * time.Millisecond) // rates need two reads
	s.lastGPU = time.Time{}
	m := s.read()
	if s.gpu.dir != "" {
		fmt.Fprintf(w, "GPU: slot %s, driver %s\n", s.gpu.slot, s.gpu.driver)
	} else {
		fmt.Fprintln(w, "GPU: none detected or disabled")
	}
	fmt.Fprintf(w, "Disk sensors: %d, system fan sensors: %d\nMetrics:\n", len(s.disks), len(s.fans))
	alert := map[string]float64{}
	for _, r := range makeRules(cfg, s.crit) {
		alert[r.Key] = r.On
	}
	for _, x := range metrics {
		if _, ok := m[x.key]; !ok {
			fmt.Fprintf(w, "  %-16s not available\n", x.key)
		}
	}
	for _, x := range ordered(m) {
		line := fmt.Sprintf("  %-16s %.1f %s", x.key, m[x.key], x.unit)
		if a, ok := alert[x.key]; ok {
			line += fmt.Sprintf("   alert at %.0f", a)
		}
		if c := s.crit[x.key]; c > 0 {
			line += fmt.Sprintf("   device limit %.0f", c)
		}
		fmt.Fprintln(w, line)
	}
	return nil
}

const briefMax = 600 // characters; meant for a hook

// cmdBrief prints one short summary: current values, active problems and the
// notable events of the last day.
func cmdBrief(w io.Writer) error {
	b, err := os.ReadFile(stateFile())
	if err != nil {
		return nil // silent when the daemon is off
	}
	var s State
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	s.clean()
	cfg, _ := loadConfig()
	if time.Since(time.Unix(s.T, 0)) > cfg.gapAfter() {
		fmt.Fprintln(w, "azor: live data is stale")
		return nil
	}
	var v []string
	for _, k := range []string{"cpu", "cpu_temp", "mem", "gpu", "gpu_temp", "gpu_power", "disk_temp"} {
		if x, ok := s.Values[k]; ok {
			l, u := label(k)
			v = append(v, fmt.Sprintf("%s %.0f%s", l, x, u))
		}
	}
	out := "azor: " + strings.Join(v, ", ")
	if len(s.Alerts) > 0 {
		out += "; ACTIVE ALERTS: " + strings.Join(s.Alerts, ", ")
	}
	if len(s.Failed) > 0 {
		out += fmt.Sprintf("; failed services: %d", len(s.Failed))
	}
	n := map[string]float64{}
	now := time.Now()
	load(dataDir(), now.Add(-24*time.Hour), now, func(r Record) {
		if r.Type == "event" && r.Kind != "start" && r.Kind != "clear" {
			n[r.Kind+" "+r.Key]++
		}
	})
	if len(n) > 0 {
		out += "; last 24h: " + names(n, "x")
	}
	if r := []rune(out); len(r) > briefMax {
		out = string(r[:briefMax-3]) + "..."
	}
	fmt.Fprintln(w, out)
	return nil
}

// cmdFiles shows which configuration files detection selects now.
func cmdFiles(w io.Writer) error {
	cfg, _ := loadConfig()
	c := newConfigScan()
	var st State
	if b, err := os.ReadFile(stateFile()); err == nil {
		json.Unmarshal(b, &st)
	}
	c.discover(running(st))
	for _, p := range append(c.all(), cfg.WatchFiles...) {
		fmt.Fprintln(w, clean(short(p)))
	}
	return nil
}
