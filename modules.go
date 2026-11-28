package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// moduleNames lists everything Azor can collect. All are on by default.
var moduleNames = []string{"cpu", "memory", "gpu", "disks", "network", "fans", "battery", "pressure",
	"processes", "services", "timers", "filesystem", "health", "desktop", "snapshots", "devices", "facts", "files", "journal", "packages"}

// rates turns growing counters into values per second.
type rates struct {
	val map[string]float64
	at  map[string]time.Time
}

func newRates() *rates { return &rates{map[string]float64{}, map[string]time.Time{}} }

func (r *rates) per(key string, v float64, now time.Time) (float64, bool) {
	old, seen := r.val[key]
	dt := now.Sub(r.at[key]).Seconds()
	r.val[key], r.at[key] = v, now
	if !seen || dt <= 0 || v < old {
		return 0, false
	}
	return (v - old) / dt, true
}

func (s *sensors) readCPU(m map[string]float64) {
	if v, ok := s.cpuBusy(); ok {
		m["cpu"] = v
	}
	var sum, top float64
	fs := s.freqFiles
	for _, f := range fs {
		if v, ok := readNum(f); ok {
			sum += v / 1000
			top = max(top, v/1000)
		}
	}
	if top > 0 {
		m["cpu_mhz"], m["cpu_mhz_top"] = sum/float64(len(fs)), top
	}
	if f := strings.Fields(readStr(s.root + "/proc/loadavg")); len(f) > 0 {
		if v, err := strconv.ParseFloat(f[0], 64); err == nil {
			m["load1"] = v
		}
	}
}

func (s *sensors) readNet(m map[string]float64, now time.Time) {
	var rx, tx float64
	n := 0
	for _, d := range s.glob("/sys/class/net/*") {
		if _, err := os.Stat(d + "/device"); err != nil {
			continue // virtual interface
		}
		a, ok1 := readNum(d + "/statistics/rx_bytes")
		b, ok2 := readNum(d + "/statistics/tx_bytes")
		if ok1 && ok2 {
			rx, tx, n = rx+a, tx+b, n+1
		}
	}
	if n == 0 {
		return
	}
	if v, ok := s.rt.per("rx", rx, now); ok {
		m["net_down"] = v / 1e6
	}
	if v, ok := s.rt.per("tx", tx, now); ok {
		m["net_up"] = v / 1e6
	}
}

var wholeDisk = regexp.MustCompile(`^(sd[a-z]+|nvme\d+n\d+|vd[a-z]+|mmcblk\d+)$`)

// diskLabel names a disk by its model, because numbers may change at boot.
// Disks of the same model keep the kernel's name.
func (s *sensors) diskLabel(dev string, now time.Time) string {
	if now.Sub(s.labelsAt) > time.Minute || s.labels == nil {
		s.labelsAt, s.labels = now, map[string]string{}
		count, model := map[string]int{}, map[string]string{}
		for _, b := range s.glob("/sys/block/*/device/model") {
			d := filepath.Base(filepath.Dir(filepath.Dir(b)))
			if m := strings.Join(strings.Fields(readStr(b)), " "); m != "" && wholeDisk.MatchString(d) {
				model[d] = clean(m)
				count[m]++
			}
		}
		for d, m := range model {
			if count[m] == 1 {
				s.labels[d] = m
				if c := nvmeCtl.FindString(d); c != "" {
					s.labels[c] = m // the temperature belongs to the controller
				}
			}
		}
	}
	if l := s.labels[dev]; l != "" {
		return l
	}
	return dev
}

var nvmeCtl = regexp.MustCompile(`^nvme\d+`)

func (s *sensors) readDisks(m map[string]float64, now time.Time) {
	for _, l := range strings.Split(readStr(s.root+"/proc/diskstats"), "\n") {
		f := strings.Fields(l)
		if len(f) < 13 || !wholeDisk.MatchString(f[2]) {
			continue
		}
		num := func(i int) float64 { v, _ := strconv.ParseFloat(f[i], 64); return v }
		p, n := "disk."+f[2]+".", "disk."+s.diskLabel(f[2], now)+"."
		if v, ok := s.rt.per(p+"r", num(5)*512, now); ok {
			m[n+"read"] = v / 1e6
		}
		if v, ok := s.rt.per(p+"w", num(9)*512, now); ok {
			m[n+"write"] = v / 1e6
		}
		if v, ok := s.rt.per(p+"b", num(12), now); ok { // milliseconds busy per second
			m[n+"busy"] = min(v/10, 100)
		}
	}
	for name, f := range s.disks {
		if v, ok := readNum(f); ok {
			m["disk."+s.diskLabel(name, now)+".temp"] = v / 1000
			m["disk_temp"] = max(m["disk_temp"], v/1000)
			// degrees above the warning point of this very disk (10 below its limit)
			over := v/1000 - (s.diskCrit[name] - 10)
			if o, seen := m["disk_over"]; !seen || over > o {
				m["disk_over"] = over
			}
		}
	}
}

var diskFS = map[string]bool{"ext4": true, "ext3": true, "btrfs": true, "xfs": true, "f2fs": true, "vfat": true, "zfs": true, "bcachefs": true, "ntfs3": true, "ntfs": true}

// filesystems reports usage once per device, so subvolumes are not repeated.
func filesystems(mounts string, usage func(string) (float64, bool)) map[string]float64 {
	out, seen := map[string]float64{}, map[string]bool{}
	for _, l := range strings.Split(mounts, "\n") {
		f := strings.Fields(l)
		if len(f) < 3 || !diskFS[f[2]] || seen[f[0]] {
			continue
		}
		if v, ok := usage(f[1]); ok {
			seen[f[0]] = true
			out["fs."+f[1]+".used"] = v
		}
	}
	return out
}

func fsUsed(p string) (float64, bool) {
	var st syscall.Statfs_t
	if syscall.Statfs(p, &st) != nil || st.Blocks == 0 {
		return 0, false
	}
	used := float64(st.Blocks - st.Bfree)
	return 100 * used / (used + float64(st.Bavail)), true
}

func (s *sensors) readBattery(m map[string]float64) {
	for _, d := range s.glob("/sys/class/power_supply/BAT*") {
		if v, ok := readNum(d + "/capacity"); ok {
			m["battery"] = v
		}
		if v, ok := readNum(d + "/power_now"); ok {
			m["battery_power"] = v / 1e6
		}
	}
}

// factsVersion changes when the set of facts changes, so that an Azor upgrade
// does not look like a change of the computer.
const factsVersion = "8"

// facts are settings and states; a change becomes an event.
func (s *sensors) facts() map[string]string {
	out := map[string]string{}
	put := func(k, v string) {
		if r := []rune(v); len(r) > 240 {
			v = string(r[:240])
		}
		if v != "" {
			out[k] = v
		}
	}
	put("facts_version", factsVersion)
	s.inventory(put)
	s.tuning(put)
	put("kernel", readStr(s.root+"/proc/sys/kernel/osrelease"))
	c := s.root + "/sys/devices/system/cpu/cpu0/cpufreq/"
	put("cpu_governor", readStr(c+"scaling_governor"))
	put("cpu_epp", readStr(c+"energy_performance_preference"))
	put("platform_profile", readStr(s.root+"/sys/firmware/acpi/platform_profile"))
	sched := readStr(s.root + "/sys/kernel/sched_ext/state")
	if o := readStr(s.root + "/sys/kernel/sched_ext/root/ops"); o != "" {
		sched = o
	}
	put("sched_ext", sched)
	if s.gpu.dir != "" && !s.gpuAsleep() {
		put("gpu_level", readStr(s.gpu.dir+"/power_dpm_force_performance_level"))
	}
	for _, f := range s.glob("/sys/class/power_supply/*/online") {
		if t := readStr(filepath.Dir(f) + "/type"); t == "Mains" {
			put("ac_power", readStr(f))
		}
	}
	if s.cfg.on("filesystem") {
		s.fsFacts(put)
	}
	return out
}

// failedUnits lists failed system and user services by name.
// A scope that cannot be read is nil: unknown, not "everything recovered".
func failedUnits() map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, scope := range []string{"system", "user"} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		b, err := exec.CommandContext(ctx, "systemctl", "--"+scope, "list-units", "--state=failed", "--plain", "--no-legend", "--no-pager").Output()
		cancel()
		if err != nil {
			out[scope] = nil
			continue
		}
		m := map[string]string{}
		for _, l := range strings.Split(string(b), "\n") {
			if f := strings.Fields(l); len(f) > 0 && unitName.MatchString(f[0]) {
				m[f[0]] = "failed"
			}
		}
		out[scope] = m
	}
	return out
}

// mergeFailed keeps the last known answer of a scope that did not answer now.
func mergeFailed(known, now map[string]map[string]string) (all map[string]string, unknown []string) {
	all = map[string]string{}
	for _, scope := range []string{"system", "user"} {
		m, ok := now[scope]
		if !ok || m == nil {
			unknown = append(unknown, scope+" services")
			m = known[scope]
		} else {
			known[scope] = m
		}
		for k, v := range m {
			all[k] = v
		}
	}
	return
}

// fileMarks notices changes of files declared by the user from size, times
// and inode. Like detected files, they are never opened.
func fileMarks(paths []string) map[string]string {
	out := map[string]string{}
	for _, p := range paths {
		if !allowed(p) { // checked again: the path may be a new link
			continue
		}
		var st syscall.Stat_t
		if syscall.Stat(p, &st) != nil {
			out[p] = "missing"
			continue
		}
		out[p] = fmt.Sprintf("%x.%x.%x.%x.%x", st.Size, st.Mtim.Nano(), st.Ctim.Nano(), st.Ino, st.Dev)
	}
	return out
}

// diff returns changed, new and vanished keys in a stable order.
func diff(old, cur map[string]string) (keys []string) {
	for k, v := range cur {
		if old[k] != v {
			keys = append(keys, k)
		}
	}
	for k := range old {
		if _, ok := cur[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return
}
