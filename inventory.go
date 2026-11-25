package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// short replaces the home directory, so paths stay readable and anonymous.
func short(p string) string {
	if h := os.Getenv("HOME"); h != "" {
		if r, ok := strings.CutPrefix(p, h+"/"); ok {
			return "~/" + r
		}
	}
	return p
}

// inventory describes the hardware without serial numbers or addresses.
func (s *sensors) inventory(put func(k, v string)) {
	r := s.root
	for _, l := range strings.Split(readStr(r+"/proc/cpuinfo"), "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "model name" {
			put("hw_cpu", strings.TrimSpace(v))
			break
		}
	}
	for _, l := range strings.Split(readStr(r+"/proc/meminfo"), "\n") {
		var kb float64
		if n, _ := fmt.Sscanf(l, "MemTotal: %f kB", &kb); n == 1 {
			put("hw_ram", fmt.Sprintf("%.0f GB", kb/(1<<20)))
		}
	}
	dmi := r + "/sys/class/dmi/id/"
	put("hw_board", strings.TrimSpace(readStr(dmi+"board_vendor")+" "+readStr(dmi+"board_name")))
	put("hw_bios", strings.TrimSpace(readStr(dmi+"bios_version")+" "+readStr(dmi+"bios_date")))
	var gpus []string
	for _, c := range s.glob("/sys/class/drm/card[0-9]*") {
		if strings.Contains(filepath.Base(c), "-") {
			continue
		}
		ven, dev := readStr(c+"/device/vendor"), readStr(c+"/device/device")
		if ven == "" {
			continue
		}
		name := pciName(r+"/usr/share/hwdata/pci.ids", strings.TrimPrefix(ven, "0x"), strings.TrimPrefix(dev, "0x"))
		if d, err := os.Readlink(c + "/device/driver"); err == nil {
			name += " [" + filepath.Base(d) + "]"
		}
		gpus = append(gpus, name)
	}
	sort.Strings(gpus)
	put("hw_gpu", strings.Join(gpus, "; "))
	var disks []string
	for _, b := range s.glob("/sys/block/*") {
		if !wholeDisk.MatchString(filepath.Base(b)) {
			continue
		}
		sec, _ := readNum(b + "/size")
		kind := "SSD"
		if readStr(b+"/queue/rotational") == "1" {
			kind = "HDD"
		}
		disks = append(disks, strings.TrimSpace(fmt.Sprintf("%s %.0f GB %s", readStr(b+"/device/model"), sec*512/1e9, kind)))
	}
	sort.Strings(disks) // device names may swap between boots
	put("hw_disks", strings.Join(disks, "; "))
}

func pciName(ids, vendor, device string) string {
	fallback := vendor + ":" + device
	f, err := os.Open(ids)
	if err != nil {
		return fallback
	}
	defer f.Close()
	in, vname := false, ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := sc.Text()
		if l == "" || l[0] == '#' {
			continue
		}
		if l[0] != '\t' {
			if in {
				break
			}
			if n, ok := strings.CutPrefix(l, vendor+"  "); ok {
				in, vname = true, n
			}
			continue
		}
		if n, ok := strings.CutPrefix(l, "\t"+device+"  "); in && ok {
			return vname + " " + n
		}
	}
	if vname != "" {
		return vname + " " + device
	}
	return fallback
}

// pacTail follows pacman.log. AUR helpers such as paru and yay install through
// pacman, so their packages are included.
type pacTail struct {
	path string
	pos  int64
}

func newPacTail(path string) *pacTail {
	p := &pacTail{path: path, pos: -1}
	if i, err := os.Stat(path); err == nil {
		p.pos = i.Size()
	}
	return p
}

// next returns a summary of the package changes since the last call.
func (p *pacTail) next() (summary, names string) {
	i, err := os.Stat(p.path)
	if err != nil {
		return
	}
	if p.pos < 0 || i.Size() < p.pos { // created or rotated
		p.pos = 0
	}
	if i.Size() == p.pos {
		return
	}
	f, err := os.Open(p.path)
	if err != nil {
		return
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(io.NewSectionReader(f, p.pos, i.Size()-p.pos), 4<<20))
	if err != nil {
		return
	}
	end := bytes.LastIndexByte(b, '\n') + 1 // keep a half-written line for later
	p.pos += int64(end)
	n := map[string]int{}
	var pk []string
	for _, l := range strings.Split(string(b[:end]), "\n") {
		if m := pacLine.FindStringSubmatch(l); m != nil {
			n[m[2]]++
			pk = append(pk, m[3])
		}
	}
	var parts []string
	for _, k := range []string{"upgraded", "installed", "downgraded", "removed"} {
		if n[k] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", k, n[k]))
		}
	}
	if len(pk) > 8 {
		pk = append(pk[:8], "...")
	}
	return strings.Join(parts, ", "), strings.Join(pk, " ")
}
