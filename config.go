package main

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed azor.conf
var sampleConfig string

// Config comes from built-in defaults, then /etc/azor/azor.conf, then the
// user's azor.conf. Hardware never has to be described here.
type Config struct {
	Sample, Interval, ProcInterval                   time.Duration
	Retention                                        int
	Off                                              map[string]bool // disabled modules
	TopN                                             int
	WatchProcs, WatchFiles, WatchUnits, IgnoreTimers []string
	GPU                                              string // auto, off or a PCI slot
	Alerts                                           map[string]float64
}

func defaults() Config {
	return Config{Sample: 2 * time.Second, Interval: 10 * time.Second, ProcInterval: time.Minute, Retention: 7,
		Off: map[string]bool{}, TopN: 5, GPU: "auto", Alerts: map[string]float64{}}
}

func (c Config) on(module string) bool { return !c.Off[module] }

func list(v string) (out []string) {
	for _, x := range strings.Split(v, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return
}

// setModules accepts "all", "all, -name" or an explicit list.
func (c *Config) setModules(v string) bool {
	off := map[string]bool{}
	names := list(v)
	if len(names) == 0 {
		return false
	}
	all := names[0] == "all"
	for _, m := range moduleNames {
		off[m] = !all
	}
	for i, n := range names {
		if i == 0 && all {
			continue
		}
		name, minus := strings.CutPrefix(n, "-")
		if _, ok := off[name]; !ok {
			return false
		}
		off[name] = minus
	}
	c.Off = off
	return true
}

func (c Config) gapAfter() time.Duration { return max(45*time.Second, 4*c.Interval) }

func configPaths() []string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return []string{"/etc/azor/azor.conf", filepath.Join(base, "azor", "azor.conf")}
}

func (c *Config) apply(text, name string) (warn []string) {
	for i, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		n := *c // a rejected line must not leave half of its value behind
		if ok && n.Alerts != nil {
			n.Alerts = cloneMap(n.Alerts)
		}
		if ok && n.set(k, v) {
			*c = n
		} else {
			warn = append(warn, fmt.Sprintf("%s:%d: ignored %q", name, i+1, l))
		}
	}
	return
}

func cloneMap[K comparable, V any](m map[K]V) map[K]V {
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (c *Config) set(k, v string) bool {
	dur := func(p *time.Duration, lo, hi time.Duration) bool {
		d, err := time.ParseDuration(v)
		if err != nil || d < lo || d > hi {
			return false
		}
		*p = d
		return true
	}
	switch k {
	case "sample":
		return dur(&c.Sample, time.Second, time.Minute)
	case "interval":
		return dur(&c.Interval, 5*time.Second, 10*time.Minute)
	case "process_interval":
		return dur(&c.ProcInterval, 10*time.Second, time.Hour)
	case "retention":
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 365 {
			return false
		}
		c.Retention = n
		return true
	case "modules":
		return c.setModules(v)
	case "top":
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 20 {
			return false
		}
		c.TopN = n
		return true
	case "watch.processes":
		c.WatchProcs = list(v)
		return len(c.WatchProcs) <= 50
	case "watch.units":
		c.WatchUnits = nil
		for _, u := range list(v) {
			if !unitName.MatchString(u) || strings.HasPrefix(u, "-") {
				return false
			}
			if !strings.Contains(u, ".") {
				u += ".service"
			}
			c.WatchUnits = append(c.WatchUnits, u)
		}
		return len(c.WatchUnits) <= 50
	case "ignore.timers":
		c.IgnoreTimers = list(v)
		return true
	case "watch.files":
		c.WatchFiles = nil
		for _, p := range list(v) {
			if r, ok := strings.CutPrefix(p, "~/"); ok {
				p = filepath.Join(os.Getenv("HOME"), r)
			}
			if !filepath.IsAbs(p) {
				return false
			}
			if !allowed(p) {
				return false // a hard limit, see forbidden.go
			}
			c.WatchFiles = append(c.WatchFiles, p)
		}
		return len(c.WatchFiles) <= 100
	case "gpu":
		c.GPU = v
		return v != ""
	}
	key, ok := strings.CutPrefix(k, "alert.")
	if !ok {
		return false
	}
	known := key == "disk_temp" // an absolute limit for the hottest disk, instead of disk_over
	for _, r := range makeRules(defaults(), nil) {
		known = known || r.Key == key
	}
	if !known {
		return false
	}
	if v == "off" {
		c.Alerts[key] = 0
		return true
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n <= 0 || n > 1000 {
		return false
	}
	c.Alerts[key] = n
	return true
}

func loadConfig() (Config, []string) {
	c := defaults()
	var warn []string
	for _, p := range configPaths() {
		b, err := os.ReadFile(p)
		if err == nil {
			warn = append(warn, c.apply(string(b), p)...)
		} else if !os.IsNotExist(err) {
			warn = append(warn, err.Error())
		}
	}
	if c.Interval < c.Sample {
		c.Interval = c.Sample
	}
	return c, warn
}

func cmdConfig(w io.Writer) error {
	c, warn := loadConfig()
	for _, p := range configPaths() {
		state := "not present"
		if _, err := os.Stat(p); err == nil {
			state = "loaded"
		}
		fmt.Fprintf(w, "# %s: %s\n", p, state)
	}
	var mods []string
	for _, m := range moduleNames {
		if c.on(m) {
			mods = append(mods, m)
		}
	}
	fmt.Fprintf(w, "modules = %s\nsample = %s\ninterval = %s\nprocess_interval = %s\nretention = %d\ntop = %d\ngpu = %s\nwatch.processes = %s\nwatch.files = %s\nwatch.units = %s\nignore.timers = %s\n",
		strings.Join(mods, ", "), c.Sample, c.Interval, c.ProcInterval, c.Retention, c.TopN, c.GPU, strings.Join(c.WatchProcs, ", "), strings.Join(c.WatchFiles, ", "), strings.Join(c.WatchUnits, ", "), strings.Join(c.IgnoreTimers, ", "))
	var ks []string
	for k := range c.Alerts {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	for _, k := range ks {
		if c.Alerts[k] <= 0 {
			fmt.Fprintf(w, "alert.%s = off\n", k)
		} else {
			fmt.Fprintf(w, "alert.%s = %g\n", k, c.Alerts[k])
		}
	}
	for _, x := range warn {
		fmt.Fprintln(w, "# warning:", x)
	}
	return nil
}
