package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// This file finds the desktop without knowing the name of any desktop: the
// process that holds the display socket, the control groups of the graphical
// session, and the packages those programs come from.

// instance identifies one run of a process; numbers alone are reused.
type instance struct {
	PID   int
	Start string
	Comm  string
}

type stat struct {
	instance
	ppid  int
	ticks uint64
}

func procStat(root string, pid int) (s stat, ok bool) {
	text := readStr(root + "/proc/" + strconv.Itoa(pid) + "/stat")
	name, start, ticks, _, good := parseProcStat(text)
	if !good {
		return
	}
	f := strings.Fields(text[strings.LastIndexByte(text, ')')+1:])
	pp, _ := strconv.Atoi(f[1])
	return stat{instance{pid, start, name}, pp, ticks}, true
}

// lockHolders maps "major:minor:inode" of flock locks to the holder.
func lockHolders(text string) map[string]int {
	out := map[string]int{}
	for _, l := range strings.Split(text, "\n") {
		f := strings.Fields(l)
		if len(f) < 6 || f[1] != "FLOCK" || f[3] != "WRITE" {
			continue
		}
		if pid, err := strconv.Atoi(f[4]); err == nil && pid > 0 {
			out[f[5]] = pid
		}
	}
	return out
}

func lockKey(path string) string {
	var st syscall.Stat_t
	if syscall.Stat(path, &st) != nil {
		return ""
	}
	dev := uint64(st.Dev)
	major := (dev>>8)&0xfff | (dev>>32)&^uint64(0xfff)
	minor := dev&0xff | (dev>>12)&^uint64(0xff)
	return fmt.Sprintf("%02x:%02x:%d", major, minor, st.Ino)
}

type server struct {
	socket string
	owner  stat
}

// waylandServers finds display servers passively, by the lock that every
// Wayland server holds next to its socket. Nothing is connected to.
func waylandServers(runtime string) (out []server) {
	holders := lockHolders(readStr("/proc/locks"))
	locks, _ := filepath.Glob(filepath.Join(runtime, "wayland-*.lock"))
	for _, l := range locks {
		sock := strings.TrimSuffix(l, ".lock")
		if i, err := os.Stat(sock); err != nil || i.Mode()&os.ModeSocket == 0 {
			continue
		}
		pid := holders[lockKey(l)]
		if pid == 0 {
			continue
		}
		if s, ok := procStat("", pid); ok {
			out = append(out, server{sock, s})
		}
	}
	// The main server is not started by another one, and is the oldest.
	owners := map[int]bool{}
	for _, s := range out {
		owners[s.owner.PID] = true
	}
	nested := func(s server) bool {
		p := s.owner.ppid
		for i := 0; i < 20 && p > 1; i++ {
			if owners[p] && p != s.owner.PID {
				return true
			}
			x, ok := procStat("", p)
			if !ok {
				break
			}
			p = x.ppid
		}
		return false
	}
	sort.SliceStable(out, func(a, b int) bool {
		na, nb := nested(out[a]), nested(out[b])
		if na != nb {
			return nb
		}
		x, _ := strconv.ParseUint(out[a].owner.Start, 10, 64)
		y, _ := strconv.ParseUint(out[b].owner.Start, 10, 64)
		return x < y
	})
	return
}

// x11Server is used only in X11 sessions: under Wayland, touching an X socket
// could start Xwayland on demand.
func x11Server() string {
	m, _ := filepath.Glob("/tmp/.X11-unix/X[0-9]*")
	for _, p := range m {
		c, err := net.DialTimeout("unix", p, time.Second)
		if err != nil {
			continue
		}
		u, ok := peer(c)
		c.Close()
		if ok && u.Pid > 1 {
			return readStr("/proc/" + strconv.Itoa(int(u.Pid)) + "/comm")
		}
	}
	return ""
}

func cgroupOf(pid int) string {
	for _, l := range strings.Split(readStr("/proc/"+strconv.Itoa(pid)+"/cgroup"), "\n") {
		if p, ok := strings.CutPrefix(l, "0::"); ok {
			return p
		}
	}
	return ""
}

var (
	autostart = regexp.MustCompile(`^app-.*@autostart\.service$`) // systemd's own naming of started programs
	// systemd's own random or numbered ending of transient units
	instNum = regexp.MustCompile(`([-@_.][0-9]+|[-@][0-9a-f]{6,}|[-@][a-z0-9]*[0-9][a-z0-9]{5,}|[-@][a-z0-9]{5,}[0-9][a-z0-9]*)+$`)
)

// unitLabel removes the parts of a transient unit name that change every time.
func unitLabel(unit string) string {
	n := strings.TrimSuffix(strings.TrimSuffix(unit, ".service"), ".scope")
	return clean(instNum.ReplaceAllString(unescape(n), ""))
}

// leaves lists the units below a control group.
func leaves(root string) (out []string) {
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return filepath.SkipDir
		}
		if d.IsDir() && p != root && (strings.HasSuffix(p, ".service") || strings.HasSuffix(p, ".scope")) {
			out = append(out, p)
			return filepath.SkipDir
		}
		return nil
	})
	return
}

// members returns the units that belong to the graphical session.
func members() map[string]bool {
	out := map[string]bool{}
	todo, done := []string{"graphical-session.target"}, map[string]bool{}
	for depth := 0; depth < 3 && len(todo) > 0; depth++ {
		b, ok := systemctl(append([]string{"--user", "show", "--no-pager", "-p", "Id,Wants,Requires,ConsistsOf,BoundBy", "--"}, todo...)...)
		if !ok {
			return out
		}
		for _, t := range todo {
			done[t] = true
		}
		todo = nil
		for _, m := range parseShow(string(b)) {
			for _, k := range []string{"Wants", "Requires", "ConsistsOf", "BoundBy"} {
				for _, u := range strings.Fields(m[k]) {
					if strings.HasSuffix(u, ".target") && !done[u] && strings.Contains(u, "session") {
						todo = append(todo, u)
					} else if strings.HasSuffix(u, ".service") && !autostart.MatchString(u) {
						out[u] = true
					}
				}
			}
		}
	}
	return out
}

type desktop struct {
	uid       int
	runtime   string
	user      string // control group of the user manager
	slice     string // control group of the user
	part      map[string]bool
	found     time.Time
	prevCPU   map[string]float64
	started   map[string]string // unit -> invocation
	owner     instance
	ownerSeen bool
	children  map[string]instance
	probed    time.Time
	probeKey  string
	outs      map[string]string
	versions  map[string]string
	verNames  string
	main      server
	kind      string
}

func newDesktop() *desktop {
	u := os.Getuid()
	s := fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice", u)
	return &desktop{uid: u, runtime: os.Getenv("XDG_RUNTIME_DIR"), slice: s, user: userCgroup(), prevCPU: map[string]float64{}}
}

func cgroupUsec(dir string) (usec float64) {
	for _, l := range strings.Split(readStr(dir+"/cpu.stat"), "\n") {
		if v, ok := strings.CutPrefix(l, "usage_usec "); ok {
			usec, _ = strconv.ParseFloat(v, 64)
		}
	}
	return
}

// scan measures the parts of the desktop: the unit of the display server,
// everything in session.slice and the services of the graphical session.
func (d *desktop) scan(seconds float64) (out []proc) {
	ownerCg := ""
	if d.main.owner.PID > 0 {
		ownerCg = "/sys/fs/cgroup" + cgroupOf(d.main.owner.PID)
	}
	cur := map[string]float64{}
	add := func(dir, name string) {
		mem, ok := readNum(dir + "/memory.current")
		if !ok {
			return
		}
		p := proc{Name: name, MB: round1(mem / (1 << 20))}
		u := cgroupUsec(dir)
		if old, seen := d.prevCPU[dir]; seen && u >= old && seconds > 0 {
			p.CPU = round1((u - old) / seconds / 1e4)
		}
		cur[dir] = u
		if p.MB >= 5 || p.CPU >= 0.5 || dir == ownerCg {
			out = append(out, p)
		}
	}
	for _, dir := range leaves(d.user) {
		unit := filepath.Base(dir)
		inSession := strings.Contains(dir, "/session.slice/") || strings.Contains(dir, "/session-graphical.slice/")
		if dir == ownerCg || inSession || d.part[unescape(unit)] || d.part[unit] {
			add(dir, unitLabel(unit))
		}
	}
	// Desktops started outside the user manager live in a session scope,
	// together with the programs they start, so only its parts are named.
	if strings.HasSuffix(ownerCg, ".scope") && !strings.HasPrefix(ownerCg, d.user) {
		add(ownerCg, "session (all programs)")
	}
	if ownerCg != "" {
		out = append(out, d.inside(ownerCg, seconds, cur)...)
	}
	d.prevCPU = cur
	sort.Slice(out, func(a, b int) bool { return out[a].MB > out[b].MB })
	if len(out) > 25 {
		out = out[:25]
	}
	return
}

// inside names the programs running in the display server's own control
// group, such as bars started by it: groups of processes below each child.
func (d *desktop) inside(cg string, seconds float64, cur map[string]float64) (out []proc) {
	all := map[int]stat{}
	for _, l := range strings.Fields(readStr(cg + "/cgroup.procs")) {
		pid, _ := strconv.Atoi(l)
		if len(all) > 2000 {
			break
		}
		if s, ok := procStat("", pid); ok {
			all[pid] = s
		}
	}
	anchor := d.main.owner.PID
	type grp struct {
		root  stat
		ticks uint64
		mb    float64
	}
	groups := map[int]*grp{}
	for pid, s := range all {
		root := s
		for i := 0; i < 50 && root.PID != anchor; i++ {
			up, ok := all[root.ppid]
			if !ok || up.PID == anchor {
				break
			}
			root = up
		}
		g := groups[root.PID]
		if g == nil {
			g = &grp{root: root}
			groups[root.PID] = g
		}
		g.ticks += s.ticks
		for _, l := range strings.Split(readStr("/proc/"+strconv.Itoa(pid)+"/status"), "\n") {
			var kb float64
			if n, _ := fmt.Sscanf(l, "RssAnon: %f kB", &kb); n == 1 {
				g.mb += kb / 1024
			}
		}
	}
	for pid, g := range groups {
		key := fmt.Sprintf("p%d@%s", pid, g.root.Start)
		cur[key] = float64(g.ticks)
		p := proc{Name: "- " + clean(g.root.Comm), MB: round1(g.mb)}
		if old, seen := d.prevCPU[key]; seen && float64(g.ticks) >= old && seconds > 0 {
			p.CPU = round1((float64(g.ticks) - old) / seconds)
		} else if !seen && seconds > 0 {
			continue // wait until it has lived for a whole interval
		}
		if p.MB >= 5 || p.CPU >= 0.5 || pid == anchor {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].MB > out[b].MB })
	if len(out) > 6 {
		out = out[:6]
	}
	return
}

// changes reports restarts of the display server and of desktop services.
func (d *desktop) changes(t int64) (ev []Record) {
	note := func(key, old, new string) {
		ev = append(ev, Record{T: t, Type: "event", Kind: "desktop", Key: key, Old: old, New: new})
	}
	now := d.main.owner.instance
	if d.ownerSeen && now != d.owner {
		switch {
		case now.PID == 0:
			note("display server", d.owner.Comm, "ended")
		case d.owner.PID == 0:
			note("display server", "none", now.Comm+" started")
		default:
			note("display server", d.owner.Comm, now.Comm+" restarted")
		}
	}
	// A launcher may hold the socket and restart the real server below it.
	kids := map[string]instance{}
	if now.PID > 0 && strings.HasSuffix(cgroupOf(now.PID), ".service") {
		for _, l := range strings.Fields(readStr("/sys/fs/cgroup" + cgroupOf(now.PID) + "/cgroup.procs")) {
			pid, _ := strconv.Atoi(l)
			if s, ok := procStat("", pid); ok && s.ppid == now.PID {
				kids[s.Comm] = s.instance
			}
		}
		if now == d.owner {
			for c, k := range kids {
				if old, ok := d.children[c]; ok && old != k {
					note(c, "running", "restarted by "+now.Comm)
				}
			}
		}
	}
	d.children, d.owner, d.ownerSeen = kids, now, true
	cur := map[string]string{}
	for u := range d.part {
		if l, err := os.Readlink(filepath.Join(d.runtime, "systemd/units/invocation:"+u)); err == nil {
			cur[u] = l
		}
	}
	var changed []string
	if d.started != nil {
		for u, v := range cur {
			if old, ok := d.started[u]; ok && old != v {
				changed = append(changed, u)
			}
		}
	}
	d.started = cur
	if len(changed) > 0 {
		sort.Strings(changed)
		b, _ := systemctl(append([]string{"--user", "show", "--no-pager", "-p", "Id,NRestarts,Result", "--"}, changed...)...)
		info := parseShow(string(b))
		for _, u := range changed {
			how := "restarted"
			if m := info[u]; m["Result"] != "" && m["Result"] != "success" {
				how = "restarted after " + m["Result"]
			} else if n := m["NRestarts"]; n != "" && n != "0" {
				how = "restarted automatically (" + n + ")"
			}
			note(unitLabel(u), "running", how)
		}
	}
	return
}

// facts describes the desktop; changes become events.
func (d *desktop) facts(now time.Time, put func(k, v string)) {
	name, kind := session()
	d.kind = kind
	put("desktop", name)
	put("session_type", kind)
	d.main = server{}
	ws := waylandServers(d.runtime)
	if len(ws) > 0 {
		d.main = ws[0]
		put("display_server", ws[0].owner.Comm)
		var more []string
		for _, s := range ws[1:] {
			more = append(more, s.owner.Comm)
		}
		put("nested_servers", strings.Join(more, " "))
	} else if kind == "x11" {
		put("display_server", x11Server())
	}
	if now.Sub(d.found) > 10*time.Minute {
		d.part, d.found = members(), now
	}
	conn := connectors("", put)
	key := conn + "|" + d.main.socket + "|" + d.main.owner.Start
	if d.main.socket != "" && (key != d.probeKey || now.Sub(d.probed) >= 5*time.Minute) {
		d.probeKey, d.probed, d.outs = key, now, map[string]string{}
		outs, err := readOutputs(d.main.socket, uint32(d.uid))
		for i, o := range outs {
			if err != nil {
				break
			}
			n := o.Name
			if n == "" {
				n = strconv.Itoa(i + 1)
			}
			v := fmt.Sprintf("%dx%d", o.W, o.H)
			if o.MHz > 0 {
				v += fmt.Sprintf(" %.2f Hz", float64(o.MHz)/1000)
			}
			d.outs["output "+n] = v
		}
	}
	if d.main.socket == "" {
		d.outs = nil
	}
	for k, v := range d.outs {
		put(k, v)
	}
	names := map[string]bool{}
	if d.main.owner.Comm != "" {
		names[d.main.owner.Comm] = true
	}
	for _, c := range d.children {
		names[c.Comm] = true
	}
	for u := range d.part {
		names[strings.TrimSuffix(u, ".service")] = true
	}
	if k := strings.Join(sortedKeys(names), " "); k != d.verNames || d.versions == nil {
		d.verNames, d.versions = k, owners("/var/lib/pacman/local", names)
	}
	for n, v := range d.versions {
		put("version "+n, v)
	}
}

// connectors reads cached connector state; reading never probes a display.
func connectors(root string, put func(k, v string)) (sig string) {
	m, _ := filepath.Glob(root + "/sys/class/drm/card*-*/status")
	sort.Strings(m)
	n := 0
	for _, f := range m {
		if readStr(f) != "connected" {
			continue
		}
		n++
		d := filepath.Dir(f)
		_, name, _ := strings.Cut(filepath.Base(d), "-")
		v := "connected"
		if readStr(d+"/enabled") == "disabled" {
			v += ", not used"
		}
		if p := strings.ToLower(readStr(d + "/dpms")); p != "" && p != "on" {
			v += ", " + p
		}
		put("connector "+name, v)
		sig += name + "=" + v + ";"
	}
	put("displays", strconv.Itoa(n))
	return
}

// owners finds the packages of programs by reading pacman's file lists as a
// stream; asking pacman itself would need several times more memory.
func owners(db string, names map[string]bool) map[string]string {
	out := map[string]string{}
	es, err := os.ReadDir(db)
	if err != nil {
		return out
	}
	r := bufio.NewReaderSize(nil, 16<<10)
	for _, e := range es {
		f, err := os.Open(filepath.Join(db, e.Name(), "files"))
		if err != nil {
			continue
		}
		r.Reset(f)
		for {
			line, err := r.ReadSlice('\n')
			if len(line) > 8 && string(line[:8]) == "usr/bin/" {
				n := strings.TrimSpace(string(line[8:]))
				if names[n] {
					p := strings.Split(e.Name(), "-")
					if len(p) >= 3 {
						out[n] = strings.Join(p[len(p)-2:], "-")
					}
				}
			} else if string(line) == "%BACKUP%\n" {
				break
			}
			if err == io.EOF || (err != nil && err != bufio.ErrBufferFull) {
				break
			}
		}
		f.Close()
	}
	return out
}

func cmdDesktop(w io.Writer) error {
	d := newDesktop()
	facts := map[string]string{}
	put := func(k, v string) {
		if v != "" {
			facts[k] = v
		}
	}
	d.facts(time.Now(), put)
	for _, k := range sortedKeys(facts) {
		fmt.Fprintf(w, "  %-28s %s\n", clean(k), clean(facts[k]))
	}
	d.scan(0)
	time.Sleep(time.Second)
	fmt.Fprintln(w, "Parts of the desktop (1 thread = 100%; \"-\" runs inside the display server's unit):")
	for _, p := range d.scan(1) {
		fmt.Fprintf(w, "  %-28s CPU %4.0f%%  RAM %6.0f MB\n", clean(p.Name), p.CPU, p.MB)
	}
	return nil
}
