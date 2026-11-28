package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// USB devices and network links. Names come from the device descriptors;
// serial numbers and addresses are not read.

func usbDevices(root string) map[string]string {
	n := map[string]int{}
	m, _ := filepath.Glob(root + "/sys/bus/usb/devices/*/idVendor")
	for _, f := range m {
		d := filepath.Dir(f)
		if readStr(d+"/bDeviceClass") == "09" { // hubs come and go with their ports
			continue
		}
		name := strings.TrimSpace(readStr(d+"/manufacturer") + " " + readStr(d+"/product"))
		if name == "" {
			name = readStr(d+"/idVendor") + ":" + readStr(d+"/idProduct")
		}
		n[cleanTo(name, 60)]++
	}
	out := map[string]string{}
	for k, v := range n {
		out[k] = fmt.Sprint(v)
	}
	return out
}

func netLinks(root string) map[string]string {
	out := map[string]string{}
	m, _ := filepath.Glob(root + "/sys/class/net/*/operstate")
	for _, f := range m {
		d := filepath.Dir(f)
		if _, err := os.Stat(d + "/device"); err != nil { // virtual: bridges, tunnels, containers
			continue
		}
		v := readStr(f)
		if s, ok := readNum(d + "/speed"); ok && s > 0 && v == "up" {
			v += fmt.Sprintf(" %.0f Mb/s", s)
		}
		out[filepath.Base(d)] = v
	}
	return out
}

// jsonOut wraps output for programs: a version, what it is and when.
func jsonOut(w io.Writer, kind string, data any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"azor_json": 1, "kind": kind, "generated": time.Now().Unix(), "data": data})
}

// cmdJSON gives the main views as JSON for agents and scripts.
func cmdJSON(w io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: azor json now|events [SPAN]|snapshots [SPAN]|timers|health")
	}
	what, rest := args[0], args[1:]
	switch what {
	case "now":
		b, err := os.ReadFile(stateFile())
		if err != nil {
			return fmt.Errorf("no live state; is the daemon running?")
		}
		var s State
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		s.clean()
		return jsonOut(w, "now", s)
	case "events", "snapshots":
		from, to, err := since(rest)
		if err != nil {
			return err
		}
		out := []Record{}
		err = loadAll(dataDir(), from, to, func(r Record) {
			if r.Type == strings.TrimSuffix(what, "s") {
				out = append(out, r)
			}
		})
		if err != nil {
			return err
		}
		return jsonOut(w, what, out)
	case "timers":
		cfg, _ := loadConfig()
		ts, ok, unk := readTimers(cfg.IgnoreTimers)
		if !ok {
			return fmt.Errorf("systemd timers are not available")
		}
		ts = mergeTimers(nil, ts, nil, knownRuns(dataDir(), time.Now(), cfg.Retention))
		return jsonOut(w, "timers", map[string]any{"timers": ts, "unknown": unk})
	case "health":
		h := newHealth("")
		_, problems := h.run(time.Now())
		checks, missing := map[string]string{}, []string{}
		for _, c := range h.checks {
			if h.found[c.name] {
				checks[c.name] = clean(h.current[c.name])
			} else {
				missing = append(missing, c.name)
			}
		}
		return jsonOut(w, "health", map[string]any{"checks": checks, "not_available": missing, "problems": problems})
	}
	return fmt.Errorf("unknown view %q", what)
}

// cmdExport prints the raw records of a span, one JSON object per line, for
// attaching to a bug report; review it before sharing.
func cmdExport(w io.Writer, args []string) error {
	from, to, err := since(args)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	return load(dataDir(), from, to, func(r Record) { enc.Encode(r) })
}
