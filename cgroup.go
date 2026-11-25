package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const systemCgroup = "/sys/fs/cgroup/system.slice"

// diskIDs maps "major:minor" to whole disks, so that traffic is not counted
// again for the layers above them.
func diskIDs(root string) map[string]bool {
	out := map[string]bool{}
	m, _ := filepath.Glob(root + "/sys/block/*/dev")
	for _, f := range m {
		if wholeDisk.MatchString(filepath.Base(filepath.Dir(f))) {
			out[readStr(f)] = true
		}
	}
	return out
}

// measure reads one control group. Files that the kernel or driver does not
// provide are simply left out.
func measure(dir string, seconds float64, prev, cur map[string]float64, disks map[string]bool) (p proc, ok bool) {
	mem, ok := readNum(dir + "/memory.current")
	if !ok {
		return
	}
	p.MB = round1(mem / (1 << 20))
	rate := func(key string, v float64) float64 {
		k := dir + "|" + key
		cur[k] = v
		if old, seen := prev[k]; seen && v >= old && seconds > 0 {
			return (v - old) / seconds
		}
		return 0
	}
	p.CPU = round1(rate("cpu", cgroupUsec(dir)) / 1e4)
	for _, l := range strings.Split(readStr(dir+"/dmem.current"), "\n") {
		if f := strings.Fields(l); len(f) == 2 && strings.Contains(filepath.Base(f[0]), "vram") {
			if v, err := strconv.ParseFloat(f[1], 64); err == nil && v >= 1<<20 {
				p.VRAM = round1(p.VRAM + v/(1<<20))
			}
		}
	}
	stall := func(kind string) float64 {
		line, _, _ := strings.Cut(readStr(dir+"/"+kind+".pressure"), "\n")
		_, t, found := strings.Cut(line, "total=")
		v, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if !found || err != nil {
			return 0
		}
		return min(round1(rate("stall-"+kind, v)/1e4), 100)
	}
	p.StallCPU, p.StallMem, p.StallIO = stall("cpu"), stall("memory"), stall("io")
	var rd, wr float64
	lines := strings.Split(readStr(dir+"/io.stat"), "\n")
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) == 0 || !disks[f[0]] {
			continue
		}
		for _, x := range f[1:] {
			k, t, _ := strings.Cut(x, "=")
			v, _ := strconv.ParseFloat(t, 64)
			switch k {
			case "rbytes":
				rd += v
			case "wbytes":
				wr += v
			}
		}
	}
	p.Read, p.Write = round1(rate("read", rd)/1e6), round1(rate("write", wr)/1e6)
	return p, true
}

// svcScan measures services, which include desktop components and containers.
type svcScan struct {
	prev  map[string]float64
	disks map[string]bool
}

func (s *svcScan) scan(root string, seconds float64) (out []proc) {
	cur, at := map[string]float64{}, map[string]int{}
	s.disks = diskIDs("")
	for _, d := range leaves(root) {
		p, ok := measure(d, seconds, s.prev, cur, s.disks)
		if !ok {
			continue
		}
		p.Name = unitLabel(filepath.Base(d))
		i, dup := at[p.Name]
		if !dup {
			at[p.Name] = len(out)
			out = append(out, p)
			continue
		}
		o := &out[i] // several copies of one program
		o.CPU, o.MB, o.VRAM = round1(o.CPU+p.CPU), round1(o.MB+p.MB), round1(o.VRAM+p.VRAM)
		o.Read, o.Write = round1(o.Read+p.Read), round1(o.Write+p.Write)
		o.StallCPU, o.StallMem, o.StallIO = max(o.StallCPU, p.StallCPU), max(o.StallMem, p.StallMem), max(o.StallIO, p.StallIO)
	}
	s.prev = cur
	sort.SliceStable(out, func(a, b int) bool { return out[a].MB > out[b].MB })
	return
}

// notable keeps the largest services and those that stand out otherwise.
func notable(all []proc, n int) (out []proc) {
	for i, p := range all {
		if i < n || p.CPU >= 5 || p.VRAM >= 100 || p.Read+p.Write >= 1 || max(p.StallCPU, p.StallMem, p.StallIO) >= 5 {
			out = append(out, p)
		}
		if len(out) == n+10 {
			break
		}
	}
	return
}

// extras describes what is not shown for every service.
func (p proc) extras() (s string) {
	if p.VRAM > 0 {
		s += fmt.Sprintf("  VRAM %.0f MB", p.VRAM)
	}
	if p.Read+p.Write >= 0.1 {
		s += fmt.Sprintf("  disk %.1f MB/s", p.Read+p.Write)
	}
	for _, x := range []struct {
		name string
		v    float64
	}{{"CPU", p.StallCPU}, {"memory", p.StallMem}, {"disk", p.StallIO}} {
		if x.v >= 1 {
			s += fmt.Sprintf("  waits for %s %.0f%%", x.name, x.v)
		}
	}
	return
}
