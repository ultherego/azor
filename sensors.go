package main

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Every source read by Azor is listed in this file. Nothing here needs root,
// and nothing reads command lines, window titles, file contents or network data.

func readStr(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readNum(p string) (float64, bool) {
	if p == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(readStr(p), 64)
	return v, err == nil
}

type gpuDev struct {
	dir, hwmon, slot, driver string
	vram                     float64
}

type sensors struct {
	cfg                 Config
	root                string // "" in production, a fixture directory in tests
	gpu                 gpuDev
	src                 map[string]string  // metric -> file
	div                 map[string]float64 // metric -> divisor
	crit                map[string]float64 // metric -> critical limit reported by the device
	disks               map[string]string
	fans, freqFiles     []string
	rt                  *rates
	lastGPU             time.Time
	nvidia              *nvidiaReader
	prevBusy, prevTotal uint64
}

func newSensors(c Config, root string) *sensors {
	s := &sensors{cfg: c, root: root, rt: newRates()}
	s.discover()
	s.cpuBusy()
	return s
}

func (s *sensors) glob(p string) []string {
	m, _ := filepath.Glob(s.root + p)
	return m
}

func (s *sensors) use(key, file string, div float64) bool {
	if _, ok := readNum(file); !ok {
		return false
	}
	s.src[key], s.div[key] = file, div
	if base, ok := strings.CutSuffix(file, "_input"); ok {
		if v, ok := readNum(base + "_crit"); ok && div == 1000 && v/div >= 50 && v/div <= 150 {
			s.crit[key] = v / div
		}
	}
	return true
}

// byLabel returns the inputs of one kind ("temp", "freq", "fan") keyed by
// their lower case label; inputs without a label use their own name.
func byLabel(hwmon, kind string) map[string]string {
	out := map[string]string{}
	m, _ := filepath.Glob(hwmon + "/" + kind + "[0-9]*_input")
	sort.Strings(m)
	for _, f := range m {
		base := strings.TrimSuffix(f, "_input")
		l := strings.ToLower(readStr(base + "_label"))
		if l == "" {
			l = filepath.Base(base)
		}
		if _, dup := out[l]; !dup {
			out[l] = f
		}
	}
	return out
}

func firstOf(m map[string]string, names ...string) string {
	for _, n := range names {
		if f := m[n]; f != "" {
			return f
		}
	}
	return ""
}

// discover finds the hardware by driver name and sensor label. It makes no
// assumption about a particular machine, and cardN or hwmonN numbers may
// change between boots.
func (s *sensors) discover() {
	s.src, s.div, s.crit = map[string]string{}, map[string]float64{}, map[string]float64{}
	s.disks, s.fans, s.gpu = map[string]string{}, nil, gpuDev{}
	s.freqFiles = s.glob("/sys/devices/system/cpu/cpu[0-9]*/cpufreq/scaling_cur_freq")
	if s.cfg.on("gpu") && s.cfg.GPU != "off" {
		s.pickGPU()
	}
	for _, d := range s.glob("/sys/class/hwmon/hwmon*") {
		switch readStr(d + "/name") {
		case "k10temp", "zenpower", "coretemp", "cpu_thermal":
			t := byLabel(d, "temp")
			if f := firstOf(t, "tctl", "package id 0", "tdie", "temp1"); f != "" && s.src["cpu_temp"] == "" {
				s.use("cpu_temp", f, 1000)
			}
		case "nvme", "drivetemp":
			f := d + "/temp1_input"
			if _, ok := readNum(f); ok {
				s.disks[diskName(d)] = f
				if c, ok := readNum(d + "/temp1_crit"); ok && c >= 50e3 && c <= 150e3 && (s.crit["disk_temp"] == 0 || c/1000 < s.crit["disk_temp"]) {
					s.crit["disk_temp"] = c / 1000
				}
			}
		case "amdgpu", "nouveau", "i915", "xe":
			// GPU fans are reported separately
		default:
			m, _ := filepath.Glob(d + "/fan[0-9]*_input")
			s.fans = append(s.fans, m...)
		}
	}
	if s.src["cpu_temp"] == "" {
		for _, z := range s.glob("/sys/class/thermal/thermal_zone*") {
			if t := readStr(z + "/type"); t == "x86_pkg_temp" || strings.HasPrefix(t, "cpu") {
				if s.use("cpu_temp", z+"/temp", 1000) {
					break
				}
			}
		}
	}
}

// pickGPU takes the configured PCI slot, or else the card with most memory.
func (s *sensors) pickGPU() {
	for _, c := range s.glob("/sys/class/drm/card[0-9]*") {
		if strings.Contains(filepath.Base(c), "-") {
			continue // connector, not a card
		}
		dev := c + "/device"
		g := gpuDev{dir: dev}
		if p, err := filepath.EvalSymlinks(dev); err == nil {
			g.slot = filepath.Base(p)
		}
		if p, err := os.Readlink(dev + "/driver"); err == nil {
			g.driver = filepath.Base(p)
		}
		g.vram, _ = readNum(dev + "/mem_info_vram_total")
		if readStr(dev+"/vendor") == "0x10de" && g.vram == 0 {
			g.vram = 4 << 30 // size unknown here: above a typical iGPU carve-out, below a large discrete card
		}
		if h, _ := filepath.Glob(dev + "/hwmon/hwmon*"); len(h) > 0 {
			g.hwmon = h[0]
		}
		switch {
		case s.cfg.GPU != "auto":
			if g.slot == s.cfg.GPU {
				s.gpu = g
			}
		case s.gpu.dir == "" || g.vram > s.gpu.vram:
			s.gpu = g
		}
	}
	g := s.gpu
	if g.dir == "" {
		return
	}
	s.use("gpu", g.dir+"/gpu_busy_percent", 1)
	if g.hwmon != "" {
		t := byLabel(g.hwmon, "temp")
		s.use("gpu_temp", firstOf(t, "edge", "pkg", "temp1", "temp2"), 1000)
		s.use("gpu_junction", firstOf(t, "junction"), 1000)
		s.use("gpu_mem_temp", firstOf(t, "mem", "vram"), 1000)
		s.use("gpu_fan", firstOf(byLabel(g.hwmon, "fan"), "fan1"), 1)
		s.use("gpu_mem_clock", firstOf(byLabel(g.hwmon, "freq"), "mclk"), 1e6)
		s.use("gpu_clock", firstOf(byLabel(g.hwmon, "freq"), "sclk"), 1e6)
		if !s.use("gpu_power", g.hwmon+"/power1_average", 1e6) {
			s.use("gpu_power", g.hwmon+"/power1_input", 1e6)
		}
	}
	s.use("gpu_vram_mb", g.dir+"/mem_info_vram_used", 1<<20)
	s.use("gpu_video", g.dir+"/vcn_busy_percent", 1)
	// Intel i915 and xe: clock here, load and power from counters in read.
	for _, f := range []string{filepath.Dir(g.dir) + "/gt/gt0/rps_act_freq_mhz", filepath.Dir(g.dir) + "/gt_act_freq_mhz", g.dir + "/tile0/gt0/freq0/act_freq"} {
		if s.use("gpu_clock", f, 1) {
			break
		}
	}
	if g.driver == "nvidia" && s.root == "" && s.nvidia == nil {
		s.nvidia = newNvidia(g.slot)
	}
}

// gpuAsleep reports a runtime-suspended card; reading it would wake it up.
func (s *sensors) gpuAsleep() bool {
	return s.gpu.dir != "" && readStr(s.gpu.dir+"/power/runtime_status") == "suspended"
}

func parseStat(line string) (busy, total uint64) {
	f := strings.Fields(line)
	if len(f) < 9 {
		return
	}
	for i, x := range f[1:9] { // user..steal; guest is already part of user
		v, _ := strconv.ParseUint(x, 10, 64)
		total += v
		if i != 3 && i != 4 { // not idle, not iowait
			busy += v
		}
	}
	return
}

func (s *sensors) cpuBusy() (float64, bool) {
	line, _, _ := strings.Cut(readStr(s.root+"/proc/stat"), "\n")
	b, t := parseStat(line)
	pb, pt := s.prevBusy, s.prevTotal
	s.prevBusy, s.prevTotal = b, t
	if pt == 0 || t <= pt || b < pb {
		return 0, false
	}
	return 100 * float64(b-pb) / float64(t-pt), true
}

// parsePSI returns the "some avg10" value.
func parsePSI(text string) (float64, bool) {
	for _, l := range strings.Split(text, "\n") {
		if !strings.HasPrefix(l, "some ") {
			continue
		}
		for _, f := range strings.Fields(l) {
			if v, ok := strings.CutPrefix(f, "avg10="); ok {
				x, err := strconv.ParseFloat(v, 64)
				return x, err == nil
			}
		}
	}
	return 0, false
}

func parseMeminfo(text, totalKey, freeKey string) (usedPct float64, ok bool) {
	var total, free float64
	for _, l := range strings.Split(text, "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseFloat(f[1], 64)
		switch f[0] {
		case totalKey:
			total = v
		case freeKey:
			free = v
		}
	}
	if total == 0 {
		return 0, false
	}
	return 100 * (total - free) / total, true
}

// diskName maps a hwmon directory to a kernel disk name such as nvme0 or sda.
func diskName(hwmon string) string {
	if m, _ := filepath.Glob(hwmon + "/device/block/*"); len(m) > 0 {
		return filepath.Base(m[0])
	}
	if p, err := filepath.EvalSymlinks(hwmon + "/device"); err == nil {
		return filepath.Base(p)
	}
	return filepath.Base(hwmon)
}

// gpuDue limits reads of a secondary card with runtime power management, so
// that polling does not keep it awake all the time.
func (s *sensors) gpuDue(now time.Time) bool {
	g := s.gpu.dir
	if g == "" || s.gpuAsleep() {
		return false
	}
	if readStr(g+"/boot_vga") == "0" && readStr(g+"/power/control") == "auto" && now.Sub(s.lastGPU) < time.Minute {
		return false
	}
	s.lastGPU = now
	return true
}

func (s *sensors) readGPU(m map[string]float64, now time.Time) {
	for k, f := range s.src {
		if strings.HasPrefix(k, "gpu") {
			if v, ok := readNum(f); ok {
				m[k] = v / s.div[k]
			}
		}
	}
	if _, ok := m["gpu"]; !ok {
		for _, f := range []string{filepath.Dir(s.gpu.dir) + "/gt/gt0/rc6_residency_ms", s.gpu.dir + "/tile0/gt0/gtidle/idle_residency_ms", s.gpu.dir + "/gt0/gtidle/idle_residency_ms"} {
			if v, ok := readNum(f); ok {
				if r, ok := s.rt.per("gpu_idle", v, now); ok {
					m["gpu"] = min(max(100-r/10, 0), 100)
				}
				break
			}
		}
	}
	if _, ok := m["gpu_power"]; !ok && s.gpu.hwmon != "" {
		if v, ok := readNum(s.gpu.hwmon + "/energy1_input"); ok {
			if w, ok := s.rt.per("gpu_energy", v, now); ok {
				m["gpu_power"] = w / 1e6
			}
		}
	}
	if s.nvidia != nil {
		s.nvidia.merge(m)
	}
}

// read returns one set of metrics; a metric that cannot be read is absent.
func (s *sensors) read() map[string]float64 {
	m, now := map[string]float64{}, time.Now()
	on := s.cfg.on
	if on("cpu") {
		s.readCPU(m)
		if f, ok := s.src["cpu_temp"]; ok {
			if v, ok := readNum(f); ok {
				m["cpu_temp"] = v / 1000
			}
		}
	}
	if on("memory") {
		mi := readStr(s.root + "/proc/meminfo")
		if v, ok := parseMeminfo(mi, "MemTotal:", "MemAvailable:"); ok {
			m["mem"] = v
		}
		if v, ok := parseMeminfo(mi, "SwapTotal:", "SwapFree:"); ok {
			m["swap"] = v
		}
	}
	if on("pressure") {
		for _, k := range []string{"cpu", "memory", "io"} {
			if v, ok := parsePSI(readStr(s.root + "/proc/pressure/" + k)); ok {
				m["psi_"+k] = v
			}
		}
	}
	if on("gpu") && s.gpuDue(now) {
		s.readGPU(m, now)
	}
	if on("disks") {
		s.readDisks(m, now)
	}
	if on("network") {
		s.readNet(m, now)
	}
	if on("battery") {
		s.readBattery(m)
	}
	if on("fans") {
		for _, f := range s.fans {
			if v, ok := readNum(f); ok {
				m["system_fan"] = max(m["system_fan"], v)
			}
		}
	}
	return m
}

type proc struct {
	Name string  `json:"name"`
	CPU  float64 `json:"cpu,omitempty"` // percent of one thread
	MB   float64 `json:"mb,omitempty"`
}

type procScan struct{ prev map[string]uint64 }

// parseProcStat takes the name from inside the outermost parentheses, as
// names may contain spaces and brackets.
func parseProcStat(s string) (name, start string, ticks uint64, rssPages float64, ok bool) {
	i, j := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if i < 0 || j < i {
		return
	}
	f := strings.Fields(s[j+1:])
	if len(f) < 22 {
		return
	}
	u, _ := strconv.ParseUint(f[11], 10, 64)
	k, _ := strconv.ParseUint(f[12], 10, 64)
	rssPages, _ = strconv.ParseFloat(f[21], 64)
	return s[i+1 : j], f[19], u + k, rssPages, true
}

// top returns the busiest and largest processes by name only (comm, at most
// 15 characters); same-named processes are added together.
func (p *procScan) top(seconds float64, n int, watch []string) (cpu, mem, watched []proc) {
	files, _ := filepath.Glob("/proc/[0-9]*/stat")
	cur := make(map[string]uint64, len(files))
	ct, rs := map[string]float64{}, map[string]float64{}
	page := float64(os.Getpagesize()) / (1 << 20)
	for _, f := range files {
		name, start, ticks, rss, ok := parseProcStat(readStr(f))
		if !ok {
			continue
		}
		key := f + "@" + start
		cur[key] = ticks
		if old, seen := p.prev[key]; seen && ticks > old && seconds > 0 {
			ct[name] += float64(ticks-old) / seconds // 100 ticks/s = percent
		}
		rs[name] += rss * page
	}
	p.prev = cur
	pick := func(m map[string]float64, min float64) (out []kv) {
		for k, v := range m {
			if v >= min {
				out = append(out, kv{k, v})
			}
		}
		sort.Slice(out, func(a, b int) bool { return out[a].v > out[b].v })
		if len(out) > n {
			out = out[:n]
		}
		return
	}
	for _, name := range watch {
		if mb, ok := rs[name]; ok {
			watched = append(watched, proc{Name: name, CPU: round1(ct[name]), MB: round1(mb)})
		}
	}
	for _, x := range pick(ct, 1) {
		cpu = append(cpu, proc{Name: x.k, CPU: round1(x.v)})
	}
	for _, x := range pick(rs, 50) {
		mem = append(mem, proc{Name: x.k, MB: round1(x.v)})
	}
	return
}

type kv struct {
	k string
	v float64
}

func round1(v float64) float64 {
	if v < 0 {
		return -round1(-v)
	}
	return float64(int64(v*10+0.5)) / 10
}
