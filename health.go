package main

import (
	"context"
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

// A check is one health signal. Detection comes first: every check is probed
// once, and only those this computer can answer, without root, are used later.
type check struct {
	name string
	// count: a counter whose growth is an event. state: a text whose change is an event.
	count func() (float64, bool)
	state func() (string, bool)
	// problem turns the current value into a short note, or nothing.
	problem func(n float64, s string) string
	settle  bool // meaningless just after boot; rechecked while it is a problem
	slow    bool // started programs or directory walks: every ten minutes only
}

type health struct {
	root    string
	checks  []check
	found   map[string]bool
	last    map[string]float64
	states  map[string]string
	slowAt  time.Time
	current map[string]string
}

func newHealth(root string) *health {
	h := &health{root: root, found: map[string]bool{}, last: map[string]float64{}, states: map[string]string{}, current: map[string]string{}}
	h.checks = h.list()
	for _, c := range h.checks {
		ok := false
		if c.count != nil {
			_, ok = c.count()
		} else {
			_, ok = c.state()
		}
		h.found[c.name] = ok
	}
	// Right after boot, systemd cannot tell the boot time until every startup
	// job has ended (a catch-up backup can take minutes): that is "not yet",
	// not "not available here".
	if _, err := exec.LookPath("systemd-analyze"); err == nil && h.root == "" {
		h.found["boot_time"] = true
	}
	return h
}

func (h *health) glob(p string) []string {
	m, _ := filepath.Glob(h.root + p)
	return m
}

// sum adds numbers from all matching files; nothing readable means "not here".
func (h *health) sum(pattern string) (total float64, ok bool) {
	for _, f := range h.glob(pattern) {
		if v, good := readNum(f); good {
			total, ok = total+v, true
		}
	}
	return
}

// perPackage counts a value that every CPU of a package repeats only once.
func (h *health) perPackage(pattern string) (total float64, ok bool) {
	seen := map[string]bool{}
	for _, f := range h.glob(pattern) {
		cpu := filepath.Dir(filepath.Dir(f))
		pkg := readStr(cpu + "/topology/physical_package_id")
		if pkg == "" {
			pkg = cpu
		}
		if seen[pkg] {
			continue
		}
		if v, good := readNum(f); good {
			seen[pkg] = true
			total, ok = total+v, true
		}
	}
	return
}

// aer adds the totals of one PCI Express error class over all devices.
func (h *health) aer(class, key string) (total float64, ok bool) {
	for _, f := range h.glob("/sys/bus/pci/devices/*/" + class) {
		for _, l := range strings.Split(readStr(f), "\n") {
			if p := strings.Fields(l); len(p) == 2 && p[0] == key {
				if v, err := strconv.ParseFloat(p[1], 64); err == nil {
					total, ok = total+v, true
				}
			}
		}
	}
	return
}

func vmstat(text, key string) (float64, bool) {
	for _, l := range strings.Split(text, "\n") {
		if p := strings.Fields(l); len(p) == 2 && p[0] == key {
			v, err := strconv.ParseFloat(p[1], 64)
			return v, err == nil
		}
	}
	return 0, false
}

func plural(n float64, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%.0f %ss", n, what)
}

func atLeastOne(what string) func(float64, string) string {
	return func(n float64, _ string) string {
		if n > 0 {
			return plural(n, what)
		}
		return ""
	}
}

// command runs a program only when it is installed.
func command(name string, args ...string) (string, bool) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, p, args...).Output()
	s := strings.TrimSpace(string(b))
	return s, err == nil && s != ""
}

func (h *health) list() []check {
	return []check{
		{name: "reboot", state: h.reboot, problem: func(_ float64, s string) string {
			if s != "not needed" {
				return "REBOOT NEEDED: " + s
			}
			return ""
		}},
		{name: "oom_kills", count: func() (float64, bool) { return vmstat(readStr(h.root+"/proc/vmstat"), "oom_kill") }, problem: func(n float64, _ string) string {
			switch {
			case n == 1:
				return "1 process killed for lack of memory since boot"
			case n > 1:
				return fmt.Sprintf("%.0f processes killed for lack of memory since boot", n)
			}
			return ""
		}},
		{name: "core_dumps", count: h.coreDumps, slow: true}, // the count only; nothing is opened
		{name: "pci_fatal", count: func() (float64, bool) { return h.aer("aer_dev_fatal", "TOTAL_ERR_FATAL") }, problem: atLeastOne("fatal PCIe error")},
		{name: "pci_nonfatal", count: func() (float64, bool) { return h.aer("aer_dev_nonfatal", "TOTAL_ERR_NONFATAL") }, problem: atLeastOne("uncorrected PCIe error")},
		{name: "pci_corrected", count: func() (float64, bool) { return h.aer("aer_dev_correctable", "TOTAL_ERR_COR") }},
		{name: "memory_ecc_errors", count: func() (float64, bool) { return h.sum("/sys/devices/system/edac/mc/mc*/ue_count") }, problem: atLeastOne("uncorrected memory error")},
		{name: "cpu_throttling", count: func() (float64, bool) {
			return h.perPackage("/sys/devices/system/cpu/cpu[0-9]*/thermal_throttle/package_throttle_count")
		}},
		{name: "device_crash_dumps", count: func() (float64, bool) {
			if _, err := os.Stat(h.root + "/sys/class/devcoredump"); err != nil {
				return 0, false
			}
			return float64(len(h.glob("/sys/class/devcoredump/devcd*"))), true
		}, problem: atLeastOne("device driver crash dump")},
		{name: "suspend_failures", count: func() (float64, bool) { return readNum(h.root + "/sys/power/suspend_stats/fail") }, problem: atLeastOne("failed suspend")},
		{name: "network_errors", count: h.netErrors},
		{name: "link_width", state: h.linkWidth, problem: func(_ float64, s string) string {
			if strings.HasPrefix(s, "reduced") {
				return "GPU PCIe link " + s
			}
			return ""
		}},
		{name: "config_to_merge", count: h.pacnew, slow: true, problem: func(n float64, _ string) string {
			if n > 0 {
				return plural(n, ".pacnew file") + " to merge"
			}
			return ""
		}},
		{name: "package_lists_age", state: h.syncAge, slow: true},
		{name: "clock_sync", slow: true, settle: true, state: func() (string, bool) { return command("timedatectl", "show", "-p", "NTPSynchronized", "--value") },
			problem: func(_ float64, s string) string {
				if s == "no" {
					return "clock not synchronized"
				}
				return ""
			}},
		{name: "power_profile", slow: true, state: powerProfile},
		{name: "boot_time", slow: true, state: h.bootTime},
	}
}

// reboot notices that the modules of the running kernel are gone, which
// happens after a kernel update.
func (h *health) reboot() (string, bool) {
	rel := readStr(h.root + "/proc/sys/kernel/osrelease")
	dirs := h.glob("/usr/lib/modules/*/modules.dep")
	if rel == "" || len(dirs) == 0 {
		return "", false // no modular kernel, or a container
	}
	var have []string
	for _, d := range dirs {
		n := filepath.Base(filepath.Dir(d))
		if n == rel {
			return "not needed", true
		}
		have = append(have, n)
	}
	sort.Strings(have)
	return "kernel " + rel + " is running, installed: " + strings.Join(have, ", "), true
}

func (h *health) coreDumps() (float64, bool) {
	es, err := os.ReadDir(h.root + "/var/lib/systemd/coredump")
	return float64(len(es)), err == nil
}

func (h *health) netErrors() (total float64, ok bool) {
	for _, d := range h.glob("/sys/class/net/*") {
		if _, err := os.Stat(d + "/device"); err != nil {
			continue
		}
		for _, f := range []string{"rx_errors", "tx_errors"} {
			if v, good := readNum(d + "/statistics/" + f); good {
				total, ok = total+v, true
			}
		}
	}
	return
}

// linkWidth looks at display adapters only. Speed is left out, because cards
// lower it on purpose when idle.
func (h *health) linkWidth() (string, bool) {
	ok := false
	for _, c := range h.glob("/sys/class/drm/card[0-9]*/device") {
		if strings.Contains(filepath.Base(filepath.Dir(c)), "-") || readStr(c+"/power/runtime_status") == "suspended" {
			continue
		}
		a, ok1 := readNum(c + "/current_link_width")
		b, ok2 := readNum(c + "/max_link_width")
		if !ok1 || !ok2 || a == 0 || b > 32 {
			continue
		}
		ok = true
		if a < b {
			return fmt.Sprintf("reduced: x%.0f of x%.0f", a, b), true
		}
	}
	return "full", ok
}

func (h *health) pacnew() (float64, bool) {
	if _, err := os.Stat(h.root + "/var/lib/pacman/local"); err != nil {
		return 0, false
	}
	n := 0.0
	filepath.WalkDir(h.root+"/etc", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return filepath.SkipDir
		}
		rel := strings.TrimPrefix(p, h.root)
		if d.IsDir() && forbidden.MatchString(rel) {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(p, ".pacnew") {
			n++
		}
		return nil
	})
	return n, true
}

func (h *health) syncAge() (string, bool) {
	var newest time.Time
	for _, f := range h.glob("/var/lib/pacman/sync/*.db") {
		if i, err := os.Stat(f); err == nil && i.ModTime().After(newest) {
			newest = i.ModTime()
		}
	}
	if newest.IsZero() {
		return "", false
	}
	return newest.Format("2006-01-02"), true
}

// powerProfile asks the service directly; its own client is a Python program.
func powerProfile() (string, bool) {
	const n = "org.freedesktop.UPower.PowerProfiles"
	s, ok := command("busctl", "--system", "--auto-start=no", "get-property", n, "/org/freedesktop/UPower/PowerProfiles", n, "ActiveProfile")
	v := strings.Trim(strings.TrimPrefix(s, "s "), `"`)
	return v, ok && v != ""
}

func (h *health) bootTime() (string, bool) {
	if v := h.states["boot_time"]; v != "" {
		return v, true // cannot change until the next boot
	}
	s, ok := command("systemd-analyze", "time")
	if !ok {
		return "", false // also while the boot is not finished yet
	}
	return bootSummary(s)
}

// bootSummary separates the time until the system is usable from the total,
// which also counts jobs that start at boot and run long, such as a backup
// catching up on a missed run.
func bootSummary(text string) (string, bool) {
	first, rest, _ := strings.Cut(text, "\n")
	i := strings.LastIndex(first, "= ")
	if i < 0 {
		return "", false
	}
	total := strings.TrimSpace(first[i+2:])
	var before float64 // firmware, loader, kernel and initrd
	for _, part := range strings.Split(first[:i], "+") {
		if p := strings.TrimSpace(part); p != "" && !strings.Contains(p, "(userspace)") {
			d, _, _ := strings.Cut(p, " (")
			before += seconds(d)
		}
	}
	for _, l := range strings.Split(rest, "\n") {
		target, after, ok := strings.Cut(l, " reached after ")
		if !ok {
			continue
		}
		d, _, _ := strings.Cut(after, " in userspace")
		ready := before + seconds(d)
		if ready <= 0 {
			break
		}
		return fmt.Sprintf("%.0fs until %s (all startup jobs: %s)", ready, strings.TrimSpace(target), total), true
	}
	return total, true
}

// seconds reads systemd durations such as "5min 44.999s", "498ms" or "1h 2min".
func seconds(d string) (sum float64) {
	for _, f := range strings.Fields(d) {
		for _, u := range []struct {
			suffix string
			mult   float64
		}{{"ms", 0.001}, {"us", 1e-6}, {"min", 60}, {"h", 3600}, {"s", 1}} {
			if v, ok := strings.CutSuffix(f, u.suffix); ok {
				if n, err := strconv.ParseFloat(v, 64); err == nil {
					sum += n * u.mult
				}
				break
			}
		}
	}
	return
}

func (h *health) uptime() float64 {
	f := strings.Fields(readStr(h.root + "/proc/uptime"))
	if len(f) == 0 {
		return 1e9
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

// run evaluates the detected checks and returns events and current problems.
func (h *health) run(now time.Time) (events []Record, problems []string) {
	slow := now.Sub(h.slowAt) >= 10*time.Minute
	if slow {
		h.slowAt = now
	}
	for _, c := range h.checks {
		if !h.found[c.name] {
			continue
		}
		n, seenN := h.last[c.name]
		s, seenS := h.states[c.name]
		if c.settle && h.uptime() < 300 {
			continue
		}
		if !c.slow || slow || (c.settle && (!seenS || c.problem(n, s) != "")) {
			if c.count != nil {
				if v, ok := c.count(); ok {
					if seenN && v > n {
						events = append(events, Record{T: now.Unix(), Type: "event", Kind: "health", Key: c.name, Old: fmt.Sprintf("%.0f", n), New: fmt.Sprintf("%.0f", v)})
					}
					n = v
					h.last[c.name] = v
					h.current[c.name] = fmt.Sprintf("%.0f", v)
				}
			} else if v, ok := c.state(); ok {
				if seenS && v != s {
					events = append(events, Record{T: now.Unix(), Type: "event", Kind: "health", Key: c.name, Old: s, New: v})
				}
				s = v
				h.states[c.name], h.current[c.name] = v, v
			}
		}
		if c.problem != nil {
			if p := c.problem(n, s); p != "" {
				problems = append(problems, p)
			}
		}
	}
	return
}

// tuning adds settings that change how the system behaves.
func (s *sensors) tuning(put func(k, v string)) {
	r := s.root
	for _, f := range []string{"/sys/devices/system/cpu/cpufreq/boost", "/sys/devices/system/cpu/cpu0/cpufreq/boost"} {
		if v := readStr(r + f); v != "" {
			put("cpu_boost", v)
			break
		}
	}
	put("cpu_driver", readStr(r+"/sys/devices/system/cpu/cpu0/cpufreq/scaling_driver"))
	for _, f := range s.glob("/sys/bus/platform/drivers/amd_x3d_vcache/*/amd_x3d_mode") {
		put("cpu_cache_mode", readStr(f))
	}
	bracket := func(t string) string {
		_, a, ok := strings.Cut(t, "[")
		b, _, ok2 := strings.Cut(a, "]")
		if ok && ok2 {
			return b
		}
		return ""
	}
	put("huge_pages", bracket(readStr(r+"/sys/kernel/mm/transparent_hugepage/enabled")))
	put("congestion_control", readStr(r+"/proc/sys/net/ipv4/tcp_congestion_control"))
	put("swappiness", readStr(r+"/proc/sys/vm/swappiness"))
	var io []string
	for _, f := range s.glob("/sys/block/*/queue/scheduler") {
		dev := filepath.Base(filepath.Dir(filepath.Dir(f)))
		if b := bracket(readStr(f)); b != "" && wholeDisk.MatchString(dev) {
			io = append(io, b)
		}
	}
	sort.Strings(io) // device names may swap between boots
	put("io_schedulers", strings.Join(io, " "))
	for _, l := range strings.Split(readStr(r+"/proc/cpuinfo"), "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "microcode" {
			put("microcode", strings.TrimSpace(v))
			break
		}
	}
	put("kernel_taint", readStr(r+"/proc/sys/kernel/tainted"))
}

// readSwap adds swap traffic and zram use.
func (s *sensors) readSwap(m map[string]float64, now time.Time) {
	vs := readStr(s.root + "/proc/vmstat")
	page := float64(os.Getpagesize()) / 1e6
	if _, hasSwap := m["swap"]; hasSwap {
		for _, x := range [][2]string{{"pswpin", "swap_in"}, {"pswpout", "swap_out"}} {
			if v, ok := vmstat(vs, x[0]); ok {
				if r, ok := s.rt.per(x[0], v, now); ok {
					m[x[1]] = r * page
				}
			}
		}
	}
	var orig, used float64
	found := false
	for _, f := range s.glob("/sys/block/zram*/mm_stat") {
		if p := strings.Fields(readStr(f)); len(p) >= 3 {
			a, _ := strconv.ParseFloat(p[0], 64)
			b, _ := strconv.ParseFloat(p[2], 64)
			orig, used, found = orig+a, used+b, true
		}
	}
	if found {
		m["zram_data_mb"], m["zram_ram_mb"] = orig/(1<<20), used/(1<<20)
	}
	for _, l := range strings.Split(readStr(s.root+"/proc/stat"), "\n") {
		if v, ok := strings.CutPrefix(l, "procs_blocked "); ok {
			if n, err := strconv.ParseFloat(v, 64); err == nil {
				m["blocked"] = n
			}
		}
	}
}

func cmdHealth(w io.Writer) error {
	h := newHealth("")
	_, problems := h.run(time.Now())
	for _, c := range h.checks {
		if h.found[c.name] {
			v := h.current[c.name]
			if v == "" && c.settle {
				v = "checked 5 minutes after boot"
			}
			fmt.Fprintf(w, "  %-20s %s\n", c.name, clean(v))
		}
	}
	var no []string
	for _, c := range h.checks {
		if !h.found[c.name] {
			no = append(no, c.name)
		}
	}
	if len(no) > 0 {
		fmt.Fprintln(w, "Not available here:", strings.Join(no, ", "))
	}
	for _, p := range problems {
		fmt.Fprintln(w, "ATTENTION:", clean(p))
	}
	if len(problems) == 0 {
		fmt.Fprintln(w, "No problems found by these checks.")
	}
	return nil
}
