package main

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Nothing in this file knows the name of a desktop, program or configuration
// file: the running system is asked instead.

// session asks logind for the desktop name and session type, so that the
// session environment, which may hold secrets, is never read.
func session() (desktop, kind string) {
	ask := func(args ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		b, err := exec.CommandContext(ctx, "loginctl", args...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	id := ask("show-user", strconv.Itoa(os.Getuid()), "-p", "Display", "--value")
	if id == "" {
		return
	}
	return parseSession(ask("show-session", id, "-p", "Type", "-p", "Desktop"))
}

func parseSession(s string) (desktop, kind string) {
	for _, l := range strings.Split(s, "\n") {
		if v, ok := strings.CutPrefix(l, "Desktop="); ok {
			desktop = v
		}
		if v, ok := strings.CutPrefix(l, "Type="); ok {
			kind = v
		}
	}
	return
}

type svcScan struct{ prev map[string]float64 }

// scan measures the user's running services, which include the desktop
// components, from their control groups.
func (s *svcScan) scan(root string, seconds float64) (out []proc) {
	cur, at := map[string]float64{}, map[string]int{}
	dirs := leaves(root)
	for _, d := range dirs {
		mem, ok := readNum(d + "/memory.current")
		if !ok {
			continue
		}
		name := unitLabel(filepath.Base(d))
		p := proc{Name: name, MB: round1(mem / (1 << 20))}
		for _, l := range strings.Split(readStr(d+"/cpu.stat"), "\n") {
			if v, ok := strings.CutPrefix(l, "usage_usec "); ok {
				u, _ := strconv.ParseFloat(v, 64)
				if old, seen := s.prev[d]; seen && u >= old && seconds > 0 {
					p.CPU = round1((u - old) / seconds / 1e4)
				}
				cur[d] = u
			}
		}
		if i, ok := at[name]; ok { // several copies of one program
			out[i].CPU, out[i].MB = round1(out[i].CPU+p.CPU), round1(out[i].MB+p.MB)
			continue
		}
		at[name] = len(out)
		out = append(out, p)
	}
	s.prev = cur
	sort.Slice(out, func(a, b int) bool { return out[a].MB > out[b].MB })
	return
}

var hexEscape = regexp.MustCompile(`\\x[0-9a-f]{2}`)

// unescape turns a systemd unit name such as a\x2db into a-b.
func unescape(s string) string {
	return hexEscape.ReplaceAllStringFunc(s, func(x string) string {
		n, err := strconv.ParseUint(x[2:], 16, 8)
		if err != nil || n < 32 {
			return x
		}
		return string(rune(n))
	})
}

func userCgroup() string {
	u := strconv.Itoa(os.Getuid())
	return "/sys/fs/cgroup/user.slice/user-" + u + ".slice/user@" + u + ".service"
}

// --- configuration files ---

var (
	configExt  = regexp.MustCompile(`(?i)(\.(conf|cfg|ini|toml|ya?ml|jsonc?|kdl|lua|css|fish|vim|xml|rules|preset|theme|service|timer|desktop|list)|rc|/config)$`)
	sensitive  = regexp.MustCompile(`(?i)(token|secret|credential|passw|passphrase|(^|[/._-])pass([/._-]|$)|auth|cookie|session|login|key(s|ring)?\b|\.pem|\.env|shadow|wallet|vault|history|cache|\.lock$|hosts\.|\bssh|sshd|gnupg|\bgpg|rclone|\bgh\b|docker|kube|\baws\b|vpn|wireguard|borg|restic|keepass|bitwarden|1password|polkit|\bpam\.d|sudo)`)
	backupName = regexp.MustCompile(`(?i)(\.bak|\.backup|\.orig|\.old|\.tmp|\.swp|~$|\.pac(new|save)$)`)
)

// profileMarks betray an application data profile rather than configuration.
var profileMarks = []string{"Cookies", "Local Storage", "IndexedDB", "Cache", "GPUCache", "databases", "profiles.ini", "prefs.js", "Preferences", "storage"}

const (
	perProgram = 60
	maxFiles   = 1500
)

func isProfile(dir string) bool {
	for _, n := range profileMarks {
		if _, err := os.Lstat(filepath.Join(dir, n)); err == nil {
			return true
		}
	}
	return false
}

func wanted(rel string) bool {
	return configExt.MatchString("/"+rel) && !sensitive.MatchString(rel) && !backupName.MatchString(rel)
}

// normal makes names such as "Foo", "foorc" and "foo-git" comparable.
func normal(s string) string {
	s = strings.ToLower(s)
	for _, x := range []string{".conf", ".toml", ".ini", ".json", ".yaml", ".yml", ".xml"} {
		s = strings.TrimSuffix(s, x)
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "rc"), "-git")
	return strings.TrimSuffix(strings.ReplaceAll(s, "-", ""), ".0") // gtk-3.0 is gtk3
}

// standard entries of the freedesktop and systemd specifications belong to no program.
var standard = []string{"autostart", "environment.d", "mimeapps.list", "user-dirs.dirs"}

// userConfigs groups configuration by program: an entry counts when its name
// matches something installed or running.
func userConfigs(home string, known map[string]bool) map[string][]string {
	out := map[string][]string{}
	es, _ := os.ReadDir(home)
	for _, e := range es {
		name := e.Name()
		if !known[normal(name)] || sensitive.MatchString(name) || forbidden.MatchString(name) {
			continue
		}
		p := filepath.Join(home, name)
		i, err := os.Stat(p)
		if err != nil {
			continue
		}
		if i.Mode().IsRegular() {
			if wanted(name) {
				out[name] = append(out[name], p)
			}
			continue
		}
		if !i.IsDir() || isProfile(p) {
			continue
		}
		p = p + "/"
		filepath.WalkDir(p, func(f string, d os.DirEntry, err error) error {
			if err != nil || len(out[name]) >= perProgram {
				return filepath.SkipDir
			}
			rel := strings.TrimPrefix(f, p)
			if d.IsDir() {
				if f != p && (strings.Count(rel, "/") >= 2 || strings.HasPrefix(d.Name(), ".") || sensitive.MatchString(d.Name()) || isProfile(f)) {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type().IsRegular() && wanted(rel) {
				out[name] = append(out[name], f)
			}
			return nil
		})
	}
	return out
}

// packages returns installed package names and the /etc files that pacman
// itself marks as configuration.
func packages(db string) (names map[string]bool, conf []string) {
	names = map[string]bool{}
	es, _ := os.ReadDir(db)
	for _, e := range es {
		f := strings.Split(e.Name(), "-")
		if len(f) < 3 {
			continue
		}
		n := strings.Join(f[:len(f)-2], "-")
		names[normal(n)] = true
		for _, part := range strings.SplitN(n, "-", 2)[:1] { // dux-shell -> dux
			if len(part) > 2 {
				names[normal(part)] = true
			}
		}
		conf = append(conf, backupList(filepath.Join(db, e.Name(), "files"))...)
	}
	return
}

func backupList(path string) (out []string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	// The section is at the end; skip the long file list of big packages.
	if i, err := f.Stat(); err == nil && i.Size() > 16<<10 {
		f.Seek(-16<<10, 2)
	}
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := sc.Text()
		if l == "%BACKUP%" {
			in = true
			continue
		}
		if in {
			p, _, _ := strings.Cut(l, "\t")
			if strings.HasPrefix(p, "etc/") && !sensitive.MatchString(p) {
				out = append(out, "/"+p)
			}
		}
	}
	return
}

// configScan remembers what to look at; the targets are found again every few minutes.
type configScan struct {
	home, db, etc string
	bins          []string
	services      *svcScan
	targets       map[string][]string // program -> files
	found         time.Time
	last          map[string]string
	noisy         map[string]int
}

func newConfigScan() *configScan {
	return &configScan{home: filepath.Dir(filepath.Dir(configPaths()[1])), db: "/var/lib/pacman/local", etc: "/etc", bins: []string{"/usr/bin", filepath.Join(os.Getenv("HOME"), ".local", "bin")}, noisy: map[string]int{}}
}

func (c *configScan) discover(running []string) {
	known, conf := packages(c.db)
	for _, n := range append(running, standard...) {
		known[normal(n)] = true
	}
	for _, dir := range c.bins { // a program may be named unlike its package
		es, _ := os.ReadDir(dir)
		for _, e := range es {
			known[normal(e.Name())] = true
		}
	}
	t := userConfigs(c.home, known)
	sys := map[string]bool{}
	for _, p := range conf {
		sys[p] = true
	}
	for _, g := range []string{"/*.d/*", "/*/*.d/*", "/default/*", "/*.conf"} {
		m, _ := filepath.Glob(c.etc + g)
		for _, p := range m {
			if !sensitive.MatchString(p) && !backupName.MatchString(p) && !strings.Contains(p, "/ssl/") && !strings.Contains(p, "/ca-") {
				sys[p] = true
			}
		}
	}
	for _, p := range sortedKeys(sys) {
		if i, err := os.Stat(p); err == nil && i.Mode().IsRegular() && len(t["/etc"]) < maxFiles {
			t["/etc"] = append(t["/etc"], p)
		}
	}
	for prog, files := range t {
		var ok []string
		for _, f := range files {
			if allowed(f) {
				ok = append(ok, f)
			}
		}
		t[prog] = ok
	}
	c.targets, c.found = t, time.Now()
}

// changes compares size and modification time only; no file is opened.
func (c *configScan) changes() (out []Record) {
	cur := map[string]string{}
	by := map[string][]string{}
	for prog, files := range c.targets {
		for _, f := range files {
			mark := "missing"
			if i, err := os.Stat(f); err == nil {
				mark = strconv.FormatInt(i.Size(), 36) + "." + strconv.FormatInt(i.ModTime().UnixNano(), 36)
			}
			cur[f] = mark
			if old, seen := c.last[f]; seen && old != mark {
				if c.noisy[f]++; c.noisy[f] <= 5 { // a file that changes all the time is not news
					by[prog] = append(by[prog], filepath.Base(f))
				}
			}
		}
	}
	c.last = cur
	for _, prog := range sortedKeys(by) {
		fs := by[prog]
		sort.Strings(fs)
		if len(fs) > 6 {
			fs = append(fs[:6], "...")
		}
		out = append(out, Record{Type: "event", Kind: "config", Key: prog, New: strings.Join(fs, " ")})
	}
	return
}

// all lists every watched file.
func (c *configScan) all() (out []string) {
	for _, p := range sortedKeys(c.targets) {
		out = append(out, c.targets[p]...)
	}
	return
}
