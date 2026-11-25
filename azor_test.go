package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParsers(t *testing.T) {
	if b, tot := parseStat("cpu 10 0 10 70 10 0 0 0 5 0"); b != 20 || tot != 100 {
		t.Fatal(b, tot)
	}
	if v, ok := parsePSI("some avg10=1.25 avg60=0.00 total=5\nfull avg10=9.00"); !ok || v != 1.25 {
		t.Fatal(v, ok)
	}
	if v, ok := parseMeminfo("MemTotal: 1000 kB\nMemAvailable: 250 kB", "MemTotal:", "MemAvailable:"); !ok || v != 75 {
		t.Fatal(v, ok)
	}
	n, st, tk, rss, ok := parseProcStat("12 (a) b (c) S 1 1 1 0 -1 0 0 0 0 0 7 3 0 0 20 0 1 0 999 4096 25 0")
	if !ok || n != "a) b (c" || st != "999" || tk != 10 || rss != 25 {
		t.Fatal(n, st, tk, rss, ok)
	}
	for _, bad := range []string{"", "1 x", "1 (x) S"} {
		if _, _, _, _, ok := parseProcStat(bad); ok {
			t.Fatal(bad)
		}
	}
}

func TestAlertHysteresis(t *testing.T) {
	a := alerts{[]rule{{"cpu_temp", 85, 78, 2}}, map[string]*alert{}}
	step := func(i int64, v float64) []Record {
		m := map[string]float64{"cpu_temp": v}
		return a.update(i, m, m)
	}
	var got []string
	for i, v := range []float64{90, 70, 90, 91, 95, 80, 77, 80, 77, 76, 70} {
		for _, e := range step(int64(i*10), v) {
			got = append(got, e.Kind)
			if e.Kind == "clear" && (e.Val != 95 || e.Secs != 60) {
				t.Fatalf("%+v", e)
			}
		}
	}
	if strings.Join(got, ",") != "alert,clear" {
		t.Fatal(got)
	}
}

func TestAggregateKeepsPeaks(t *testing.T) {
	a := newAgg()
	for _, v := range []float64{50, 100, 60} {
		a.add(map[string]float64{"x": v, "flat": 5})
	}
	avg, mx := a.result()
	if avg["x"] != 70 || mx["x"] != 100 || withMax(avg, mx)["flat"] != 5 {
		t.Fatal(avg, mx)
	}
	if _, stored := mx["flat"]; stored {
		t.Fatal("redundant maximum stored")
	}
}

func TestStoreLoadPrune(t *testing.T) {
	d := t.TempDir()
	now := time.Now()
	if err := appendRecord(d, Record{T: now.Unix(), Type: "sample", Avg: map[string]float64{"cpu": 1}}); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(dayPath(d, now), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("{broken\n{\"v\":99,\"t\":1}\n")
	f.Close()
	i, _ := os.Stat(dayPath(d, now))
	if i.Mode().Perm() != 0o600 {
		t.Fatal(i.Mode())
	}
	n := 0
	load(d, now.Add(-time.Minute), now.Add(time.Minute), func(Record) { n++ })
	if n != 1 {
		t.Fatal(n)
	}
	old := filepath.Join(d, "2001-01-01.jsonl")
	keep := filepath.Join(d, "notes.txt")
	os.WriteFile(old, nil, 0o600)
	os.WriteFile(keep, nil, 0o600)
	prune(d, now, 7)
	if _, err := os.Stat(old); err == nil {
		t.Fatal("old file kept")
	}
	for _, p := range []string{keep, dayPath(d, now)} {
		if _, err := os.Stat(p); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTimes(t *testing.T) {
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.Local)
	if x, _ := parseTime("12:00", now); x.Day() != 24 {
		t.Fatal(x)
	}
	f, to, err := parseRange("22:00", "01:00", now)
	if err != nil || to.Sub(f) != 3*time.Hour {
		t.Fatal(f, to, err)
	}
	f, to, err = parseRange("09:00", "now", now)
	if err != nil || to.Sub(f) != time.Hour {
		t.Fatal(f, to, err)
	}
	if _, _, err := parseRange("2026-11-25 09:00", "2026-11-25 08:00", now); err == nil {
		t.Fatal("reversed range accepted")
	}
	if _, err := parseTime("25:99", now); err == nil {
		t.Fatal("bad time accepted")
	}
}

func TestPacman(t *testing.T) {
	p := filepath.Join(t.TempDir(), "l")
	os.WriteFile(p, []byte("[2026-11-24T10:00:00+0100] [ALPM] upgraded mesa (1 -> 2)\n[2026-11-24T10:00:00+0100] [PACMAN] Running 'x secret'\n"), 0o600)
	var b bytes.Buffer
	at, _ := time.Parse(time.RFC3339, "2026-11-24T10:00:00+01:00")
	pacman(&b, p, at.Add(-time.Hour), at.Add(time.Hour))
	if !strings.Contains(b.String(), "upgraded mesa") || strings.Contains(b.String(), "secret") {
		t.Fatal(b.String())
	}
}

func put(t *testing.T, root string, files map[string]string) {
	for p, v := range files {
		p = filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Fixtures use invented values and different numbering on purpose.
func TestDiscoveryIsGeneric(t *testing.T) {
	r := t.TempDir()
	put(t, r, map[string]string{
		"sys/class/hwmon/hwmon7/name": "coretemp", "sys/class/hwmon/hwmon7/temp1_input": "41000", "sys/class/hwmon/hwmon7/temp1_label": "Package id 0", "sys/class/hwmon/hwmon7/temp1_crit": "105000",
		"sys/class/hwmon/hwmon9/name": "drivetemp", "sys/class/hwmon/hwmon9/temp1_input": "33000",
		"sys/class/hwmon/hwmon2/name": "board", "sys/class/hwmon/hwmon2/fan3_input": "900",
		"sys/class/drm/card0/device/vendor":              "0x8086",
		"sys/class/drm/card3/device/mem_info_vram_total": "1000", "sys/class/drm/card3/device/gpu_busy_percent": "12",
		"sys/class/drm/card3/device/hwmon/hwmon5/temp2_input": "55000", "sys/class/drm/card3/device/hwmon/hwmon5/temp2_label": "junction", "sys/class/drm/card3/device/hwmon/hwmon5/temp2_crit": "120000",
		"sys/class/drm/card3-DP-1/status": "connected",
	})
	s := newSensors(defaults(), r)
	m := s.read()
	for k, v := range map[string]float64{"cpu_temp": 41, "disk_temp": 33, "system_fan": 900, "gpu": 12, "gpu_junction": 55} {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
	if _, ok := m["gpu_power"]; ok {
		t.Error("missing sensor reported")
	}
	got := map[string]float64{}
	for _, x := range makeRules(defaults(), s.crit) {
		got[x.Key] = x.On
	}
	if got["cpu_temp"] != 95 || got["gpu_junction"] != 110 || got["disk_temp"] != 75 {
		t.Error(got)
	}
	put(t, r, map[string]string{"sys/class/drm/card3/device/power/runtime_status": "suspended"})
	if _, ok := s.read()["gpu"]; ok {
		t.Error("sleeping GPU was read")
	}
	c := defaults()
	c.GPU = "off"
	if _, ok := newSensors(c, r).read()["gpu"]; ok {
		t.Error("disabled GPU was read")
	}
	if len(newSensors(defaults(), t.TempDir()).read()) != 0 {
		t.Error("empty machine")
	}
}

func TestConfig(t *testing.T) {
	c := defaults()
	w := c.apply("# c\ninterval = 30s\nretention = 14\nmodules = all, -processes\nalert.cpu_temp = 80\nalert.mem = off\nbogus = 1\nsample = 0s\nretention = 9999\nnonsense", "x")
	if c.Interval != 30*time.Second || c.Retention != 14 || c.on("processes") || !c.on("gpu") || c.Alerts["cpu_temp"] != 80 || len(w) != 4 {
		t.Fatal(c, w)
	}
	for _, r := range makeRules(c, nil) {
		if r.Key == "mem" {
			t.Fatal("disabled alert present")
		}
	}
}

func TestNvidiaParse(t *testing.T) {
	m := parseNvidia("37, 61, 88.50, [N/A], 810, 1200, 900, N/A\n", nvidiaFull)
	if m["gpu"] != 37 || m["gpu_temp"] != 61 || m["gpu_power"] != 88.5 || m["gpu_mem_clock"] != 810 {
		t.Fatal(m)
	}
	if _, ok := m["gpu_fan_pct"]; ok || len(parseNvidia("garbage", nvidiaFull)) != 0 {
		t.Fatal(m)
	}
}

func TestHybridPicksDiscreteNvidia(t *testing.T) {
	r := t.TempDir()
	put(t, r, map[string]string{
		"sys/class/drm/card1/device/vendor": "0x1002", "sys/class/drm/card1/device/mem_info_vram_total": "536870912", "sys/class/drm/card1/device/gpu_busy_percent": "5",
		"sys/class/drm/card2/device/vendor": "0x10de",
	})
	if s := newSensors(defaults(), r); filepath.Base(filepath.Dir(s.gpu.dir)) != "card2" {
		t.Fatal(s.gpu.dir)
	}
}

func TestIntelLoadAndPower(t *testing.T) {
	r := t.TempDir()
	c := "sys/class/drm/card4/"
	put(t, r, map[string]string{c + "device/vendor": "0x8086", c + "gt/gt0/rc6_residency_ms": "1000", c + "gt/gt0/rps_act_freq_mhz": "700", c + "device/hwmon/hwmon1/energy1_input": "1000000"})
	s := newSensors(defaults(), r)
	s.read()
	for k := range s.rt.at {
		s.rt.at[k] = s.rt.at[k].Add(-10 * time.Second)
	}
	put(t, r, map[string]string{c + "gt/gt0/rc6_residency_ms": "8500", c + "device/hwmon/hwmon1/energy1_input": "51000000"})
	m := s.read()
	if m["gpu"] < 24 || m["gpu"] > 26 || m["gpu_power"] < 4.9 || m["gpu_power"] > 5.1 || m["gpu_clock"] != 700 {
		t.Fatal(m)
	}
}

func TestSecondaryGPUIsPolledSlowly(t *testing.T) {
	r := t.TempDir()
	c := "sys/class/drm/card0/device/"
	put(t, r, map[string]string{c + "vendor": "0x1002", c + "gpu_busy_percent": "3", c + "boot_vga": "0", c + "power/control": "auto"})
	s := newSensors(defaults(), r)
	if _, ok := s.read()["gpu"]; !ok {
		t.Fatal("first read missing")
	}
	if _, ok := s.read()["gpu"]; ok {
		t.Fatal("polled too often")
	}
}

func TestModulesAndWatch(t *testing.T) {
	c := defaults()
	if !c.setModules("cpu, gpu") || !c.on("cpu") || c.on("disks") || c.setModules("nope") || c.setModules("") {
		t.Fatal(c.Off)
	}
	t.Setenv("HOME", "/tmp/h")
	if w := c.apply("watch.processes = a, b\nwatch.files = ~/x, /y\nwatch.files = rel", "f"); len(w) != 1 || len(c.WatchProcs) != 2 {
		t.Fatal(w, c)
	}
	d := t.TempDir()
	p := filepath.Join(d, "f")
	a := fileMarks([]string{p})
	os.WriteFile(p, []byte("topsecret"), 0o600)
	b := fileMarks([]string{p})
	if a[p] != "missing" || len(diff(a, b)) != 1 || strings.Contains(b[p], "topsecret") {
		t.Fatal(a, b)
	}
}

func TestFilesystemsOncePerDevice(t *testing.T) {
	m := filesystems("/dev/a / btrfs rw\n/dev/a /home btrfs rw\ntmpfs /tmp tmpfs rw\n/dev/b /data ext4 rw", func(string) (float64, bool) { return 10, true })
	if len(m) != 2 || m["fs./.used"] != 10 {
		t.Fatal(m)
	}
}

func TestFactsSurviveRestart(t *testing.T) {
	d, now := t.TempDir(), time.Now()
	appendRecord(d, Record{T: now.Unix() - 2, Type: "facts", Facts: map[string]string{"kernel": "1", "x": "a"}})
	appendRecord(d, Record{T: now.Unix() - 1, Type: "event", Kind: "fact", Key: "kernel", Old: "1", New: "2"})
	if f := lastFacts(d, now, 7); f["kernel"] != "2" || f["x"] != "a" {
		t.Fatal(f)
	}
}

func TestUserConfigsByDetection(t *testing.T) {
	d := t.TempDir()
	put(t, d, map[string]string{"Foo/config.kdl": "a", "Foo/config.kdl.bak-1": "a", "Foo/token.json": "a", "Foo/.git/x.conf": "a", "Foo/a/b/c/deep.conf": "a", "Foo/pic.png": "a",
		"absent/a.conf": "a", "barrc": "a", "browser/Cookies": "a", "browser/settings.json": "a", "credentials/x.conf": "a"})
	got := userConfigs(d, map[string]bool{"foo": true, "bar": true, "browser": true, "credentials": true})
	if len(got) != 2 || len(got["Foo"]) != 1 || len(got["barrc"]) != 1 {
		t.Fatal(got)
	}
}

func TestSensitiveNames(t *testing.T) {
	for _, p := range []string{"etc/x/passphrase", "rclone.conf", "a/.env", "ssh_config", "etc/pam.d/x", "passwd", "gh", "wg/private.key", "sudoers", "pass"} {
		if !sensitive.MatchString(p) {
			t.Errorf("not skipped: %s", p)
		}
	}
	for _, p := range []string{"compass.conf", "bypass-x.toml", "keyboard.conf", "gtk.css", "config.kdl", "etc/default/example"} {
		if sensitive.MatchString(p) {
			t.Errorf("skipped: %s", p)
		}
	}
	if normal("gtk-3.0") != normal("gtk3") || normal("Foorc") != normal("foo") {
		t.Error("normal")
	}
}

func TestPackagesAndBackups(t *testing.T) {
	d := t.TempDir()
	put(t, d, map[string]string{"foo-bar-1.0-2/files": "%FILES%\netc/foo.conf\n\n%BACKUP%\netc/foo.conf\tabc\netc/shadow\tabc\nusr/x\tabc"})
	n, c := packages(d)
	if !n["foobar"] || len(c) != 1 || c[0] != "/etc/foo.conf" {
		t.Fatal(n, c)
	}
}

func TestConfigChangesUseMetadataOnly(t *testing.T) {
	h := t.TempDir()
	put(t, h, map[string]string{"foo/a.conf": "1"})
	f := filepath.Join(h, "foo/a.conf")
	c := &configScan{home: h, db: t.TempDir(), bins: nil, etc: t.TempDir(), noisy: map[string]int{}}
	c.discover([]string{"foo"})
	if len(c.changes()) != 0 {
		t.Fatal("first scan must be silent")
	}
	os.Chmod(f, 0) // unreadable, still watched
	n := 0
	for i := 1; i <= 9; i++ {
		at := time.Unix(int64(i), 0)
		os.Chtimes(f, at, at)
		n += len(c.changes())
	}
	if n != 5 {
		t.Fatal("noise limit", n)
	}
}

func TestServicesFromCgroups(t *testing.T) {
	r := t.TempDir()
	put(t, r, map[string]string{"session.slice/some.service/memory.current": "104857600", "session.slice/some.service/cpu.stat": "usage_usec 1000000", "app.slice/app-x\\x2dy.service/memory.current": "1048576"})
	s := &svcScan{}
	s.scan(r, 0)
	put(t, r, map[string]string{"session.slice/some.service/cpu.stat": "usage_usec 2000000"})
	o := s.scan(r, 10)
	if len(o) != 2 || o[0].Name != "some" || o[0].MB != 100 || o[0].CPU != 10 || o[1].Name != "app-x-y" {
		t.Fatal(o)
	}
}

func TestPeerNameOfOwnSocket(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s")
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if n := peerName(p); n != readStr("/proc/self/comm") {
		t.Fatal(n)
	}
	if peerName(p+"x") != "" {
		t.Fatal("missing socket")
	}
}

func TestPacTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "l")
	line := "[2026-01-01T00:00:00+0000] [ALPM] upgraded foo (1 -> 2)\n"
	os.WriteFile(p, []byte(line), 0o600)
	x := newPacTail(p)
	if s, _ := x.next(); s != "" {
		t.Fatal("old entries reported")
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(line + "[2026-01-01T00:00:00+0000] [ALPM] inst")
	if s, n := x.next(); s != "upgraded 1" || n != "foo" {
		t.Fatal(s, n)
	}
	f.WriteString("alled bar (1)\n")
	f.Close()
	if s, n := x.next(); s != "installed 1" || n != "bar" {
		t.Fatal(s, n)
	}
	os.WriteFile(p, []byte("[2026-01-01T00:00:00+0000] [ALPM] removed z (1)\n"), 0o600)
	if s, _ := x.next(); s != "removed 1" {
		t.Fatal("rotation", s)
	}
}

func TestInventory(t *testing.T) {
	r := t.TempDir()
	put(t, r, map[string]string{"proc/cpuinfo": "model name\t: Example CPU", "proc/meminfo": "MemTotal: 16777216 kB",
		"sys/block/sdz/size": "2000000000", "sys/block/sdz/device/model": "Example", "sys/block/sdz/device/serial": "S3CRET", "sys/block/sdz/queue/rotational": "1", "sys/block/loop0/size": "1",
		"sys/class/drm/card9/device/vendor": "0xaaaa", "sys/class/drm/card9/device/device": "0x0001", "usr/share/hwdata/pci.ids": "# x\naaaa  Vendor\n\t0001  Model\nbbbb  Other"})
	m := (&sensors{cfg: defaults(), root: r, rt: newRates()}).facts()
	want := map[string]string{"hw_cpu": "Example CPU", "hw_ram": "16 GB", "hw_disks": "Example 1024 GB HDD", "hw_gpu": "Vendor Model"}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	for _, v := range m {
		if strings.Contains(v, "S3CRET") {
			t.Error("serial leaked")
		}
	}
}

func TestSessionAndBoots(t *testing.T) {
	if d, k := parseSession("Type=wayland\nDesktop=foo"); d != "foo" || k != "wayland" {
		t.Fatal(d, k)
	}
	if o := oldestBoot([]byte(`[{"first_entry":2000000},{"first_entry":1000000}]`)); o.Unix() != 1 {
		t.Fatal(o)
	}
	if !oldestBoot([]byte("x")).IsZero() {
		t.Fatal("garbage accepted")
	}
}

func TestUntrustedNames(t *testing.T) {
	evil := "\x1b[31mA\nB\u202eC\u200bD\x7f\u009b"
	if g := clean(evil); strings.ContainsAny(g, "\x1b\n\x7f\u202e\u200b\u009b") || !strings.Contains(g, "A?B?C?D") {
		t.Fatalf("%q", g)
	}
	if len([]rune(cleanTo(strings.Repeat("x", 999), 80))) != 80 || clean("a\xffb") != "a?b" {
		t.Fatal("limit")
	}
	d, now := t.TempDir(), time.Now()
	appendRecord(d, Record{T: now.Unix(), Type: "event", Kind: "config", Key: evil, New: evil, CPU: []proc{{Name: evil}}, Facts: map[string]string{evil: evil}})
	b, _ := os.ReadFile(dayPath(d, now))
	if bytes.Contains(b, []byte("u001b")) || bytes.Count(b, []byte("\n")) != 1 {
		t.Fatalf("%s", b)
	}
	f, _ := os.OpenFile(dayPath(d, now), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"v":1,"t":` + strconv.FormatInt(now.Unix(), 10) + `,"type":"event","kind":"config","key":"x\u001b\nforged"}` + "\n")
	f.Close()
	load(d, now.Add(-time.Minute), now.Add(time.Minute), func(r Record) {
		if strings.ContainsAny(eventLine(r), "\x1b\n\u202e") {
			t.Fatalf("%q", eventLine(r))
		}
	})
}

func TestTimers(t *testing.T) {
	ts := parseTimers([]byte(`[{"next":5000000,"left":1,"last":3000000,"passed":1,"unit":"a.timer","activates":"a.service"},{"next":null,"last":0,"unit":"b.timer","activates":"b.service"},{"unit":"x\u001b.timer","activates":"c"}]`), false)
	if len(ts) != 2 || ts[0].Last != 3 || ts[0].Next != 5 || ts[1].Last != 0 {
		t.Fatal(ts)
	}
	info := parseShow("Id=a.service\nResult=exit-code\nActiveState=failed\n\nId=b.service\nResult=success\nActiveState=activating\n")
	ts[0].Result, ts[1].Result = result(info["a.service"]), result(info["b.service"])
	if !ts[0].failed() || ts[1].failed() || ts[1].Result != "running" {
		t.Fatal(ts)
	}
	st := timerStates(ts)
	if len(st) != 1 || st["a"] != "failed: exit-code" || len(failedTimers(ts)) != 1 {
		t.Fatal(st)
	}
}

func TestFilesystemErrors(t *testing.T) {
	r := t.TempDir()
	put(t, r, map[string]string{"sys/fs/btrfs/uuid/devinfo/1/error_stats": "write_errs 0\ncorruption_errs 3", "sys/fs/btrfs/uuid/devices/sdz1/x": "", "sys/fs/ext4/sdy1/errors_count": "0", "sys/block/md0/md/degraded": "1"})
	m := map[string]string{}
	(&sensors{root: r}).fsFacts(func(k, v string) { m[k] = v })
	if m["btrfs_errors sdz1"] != "corruption 3" || m["ext4_errors sdy1"] != "0" || m["raid_degraded md0"] != "1" {
		t.Fatal(m)
	}
}

func TestUserConfigAdditionsAndInit(t *testing.T) {
	c := defaults()
	if w := c.apply("watch.units = foo, bar.timer\nignore.timers = x\nwatch.units = --bad", "f"); len(w) != 1 || c.IgnoreTimers[0] != "x" {
		t.Fatal(w, c)
	}
	d := defaults()
	if w := d.apply(sampleConfig, "sample"); len(w) != 0 {
		t.Fatal(w)
	}
	var on []string // every commented setting of the sample must be valid
	for _, l := range strings.Split(sampleConfig, "\n") {
		if strings.HasPrefix(l, "#") && !strings.HasPrefix(l, "# ") && strings.Contains(l, " = ") {
			on = append(on, l[1:])
		}
	}
	if w := d.apply(strings.Join(on, "\n"), "sample"); len(w) != 0 || len(on) < 10 {
		t.Fatal(w, on)
	}
	h := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", h)
	var b bytes.Buffer
	if err := cmdInit(&b); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(h, "azor", "azor.conf")
	os.WriteFile(p, []byte("retention = 3"), 0o600)
	cmdInit(&b)
	if readStr(p) != "retention = 3" {
		t.Fatal("overwritten")
	}
}

func TestTimerNotRunSinceBootIsUnknown(t *testing.T) {
	x := timer{Name: "a", Last: 5, Result: result(map[string]string{"Result": "success", "ActiveState": "inactive"})}
	if x.Result != "unknown" || x.failed() || len(timerStates([]timer{x})) != 0 {
		t.Fatal(x)
	}
	if r := result(map[string]string{"Result": "success", "ActiveState": "inactive", "ExecMainExitTimestamp": "@5"}); r != "success" {
		t.Fatal(r)
	}
}

func TestForbiddenIsAHardLimit(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	for _, p := range []string{"~/.ssh/config", "~/.ssh", "~/.gnupg/gpg.conf", "~/.local/share/keyrings/x", "~/.password-store/a", "~/.config/1Password/a", "~/.config/Bitwarden/data.json",
		"~/.config/rclone/rclone.conf", "~/.config/gh/hosts.yml", "~/.aws/credentials", "~/.kube/config", "~/.mozilla/x", "~/.config/chromium/Default/x", "~/.cache/cliphist/db", "~/.netrc",
		"~/project/.env", "~/project/.env.local", "/etc/shadow", "/etc/sudoers", "/etc/pam.d/su", "/etc/wireguard/wg0.conf", "/etc/borgmatic.d/passphrase", "/etc/ssh/sshd_config", "~/.bash_history", "~/a.pem", "~/db.kdbx", "~/my-passwords.txt", "~/x/api_token", "~/.docker/config.json"} {
		c := defaults()
		if w := c.apply("watch.files = "+p, "f"); len(w) != 1 || len(c.WatchFiles) != 0 {
			t.Errorf("accepted %s", p)
		}
	}
	for _, p := range []string{"/etc/pacman.conf", "/etc/fstab", "~/.config/foo/config.kdl", "~/scripts/compass.sh", "~/page.conf", "~/message.conf"} {
		c := defaults()
		if w := c.apply("watch.files = "+p, "f"); len(w) != 0 {
			t.Errorf("refused %s", p)
		}
	}
	put(t, h, map[string]string{".ssh/x": "1"})
	link := filepath.Join(h, "innocent.conf")
	os.Symlink(filepath.Join(h, ".ssh/x"), link)
	if len(fileMarks([]string{link})) != 0 || allowed(link) {
		t.Error("symlink escapes the limit")
	}
	put(t, h, map[string]string{"cfg/foo/a.conf": "1", "cfg/rclone/r.conf": "1", "cfg/ssh/a.conf": "1"})
	s := &configScan{home: filepath.Join(h, "cfg"), db: t.TempDir(), etc: t.TempDir(), noisy: map[string]int{}}
	s.discover([]string{"foo", "rclone", "ssh"})
	if len(s.all()) != 1 {
		t.Error(s.all())
	}
}

// Azor must not gain network, clipboard or write access by accident.
func TestSourceStaysHarmless(t *testing.T) {
	fs, _ := filepath.Glob("*.go")
	for _, f := range fs {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, bad := range []string{"net/http", "\"tcp", "\"udp", "wl-paste", "xclip", "xsel", "wl-copy", "org.freedesktop.secrets", "secret-tool", "/environ\"", "/cmdline\"", "/fd/", "os.Chmod", "os.Chown"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s contains %s", f, bad)
			}
		}
	}
}

func TestHealthDetectsFirst(t *testing.T) {
	h := newHealth(t.TempDir()) // an empty computer answers nothing
	for _, c := range h.checks {
		if h.found[c.name] && c.name != "clock_sync" && c.name != "power_profile" && c.name != "boot_time" {
			t.Errorf("%s found on an empty computer", c.name)
		}
	}
	r := t.TempDir()
	put(t, r, map[string]string{"proc/sys/kernel/osrelease": "1.0", "usr/lib/modules/2.0/modules.dep": "", "proc/vmstat": "oom_kill 0",
		"sys/bus/pci/devices/a/aer_dev_fatal": "X 0\nTOTAL_ERR_FATAL 0", "sys/class/drm/card7/device/current_link_width": "8", "sys/class/drm/card7/device/max_link_width": "16",
		"var/lib/pacman/local/x": "", "etc/a.conf.pacnew": "", "etc/ssh/b.pacnew": "", "var/lib/systemd/coredump/core.x": "secret"})
	h = newHealth(r)
	now := time.Now()
	ev, pr := h.run(now)
	got := strings.Join(pr, "|")
	if len(ev) != 0 || !strings.Contains(got, "REBOOT NEEDED: kernel 1.0") || !strings.Contains(got, "x8 of x16") || !strings.Contains(got, "1 .pacnew") {
		t.Fatal(ev, pr)
	}
	if h.found["cpu_throttling"] || h.found["suspend_failures"] || !h.found["oom_kills"] {
		t.Fatal(h.found)
	}
	put(t, r, map[string]string{"proc/vmstat": "oom_kill 2", "usr/lib/modules/1.0/modules.dep": ""})
	ev, pr = h.run(now.Add(time.Minute))
	if len(ev) != 2 || !strings.Contains(strings.Join(pr, "|"), "2 processes killed") || strings.Contains(strings.Join(pr, "|"), "REBOOT") {
		t.Fatal(ev, pr)
	}
}

func TestSwapAndTuning(t *testing.T) {
	r := t.TempDir()
	put(t, r, map[string]string{"proc/stat": "cpu 1 1 1 1 1 1 1 1\nprocs_blocked 3", "proc/meminfo": "MemTotal: 10 kB\nMemAvailable: 5 kB\nSwapTotal: 10 kB\nSwapFree: 5 kB",
		"sys/block/zram0/mm_stat": "1048576 1 2097152 0", "sys/block/sdz/queue/scheduler": "none [foo] bar", "sys/kernel/mm/transparent_hugepage/enabled": "always [madvise] never"})
	s := newSensors(defaults(), r)
	m := s.read()
	if m["blocked"] != 3 || m["zram_data_mb"] != 1 || m["zram_ram_mb"] != 2 {
		t.Fatal(m)
	}
	f := s.facts()
	if f["io_schedulers"] != "foo" || f["huge_pages"] != "madvise" || f["cpu_boost"] != "" {
		t.Fatal(f)
	}
}
