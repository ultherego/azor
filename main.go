// Azor is a small desktop telemetry recorder: a daemon that stores hardware
// history as JSONL, and a query command that explains what happened when.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

const usage = `azor - desktop telemetry recorder

Usage:
  azor                      current state
  azor range FROM TO        what happened in a time range
  azor peaks [DURATION]     highest values (default 24h)
  azor events [DURATION]    alerts, gaps and starts (default 24h)
  azor status               daemon health and storage
  azor brief                one short line (for hooks)
  azor hardware             detected sensors and alert limits
  azor health              detected health checks and problems
  azor timers               scheduled jobs and their last result
  azor init                 create ~/.config/azor/azor.conf
  azor files                detected configuration files
  azor config               effective configuration
  azor daemon               run the collector (used by systemd)

Times: HH:MM, "YYYY-MM-DD HH:MM", or now. Durations: 30m, 6h, 72h.
`

func dataDir() string {
	if d := os.Getenv("AZOR_DATA"); d != "" {
		return d
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		base = filepath.Join(os.Getenv("HOME"), ".local", "state")
	}
	return filepath.Join(base, "azor")
}

func stateFile() string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if os.Getenv("AZOR_DATA") != "" || base == "" {
		base = dataDir()
	} else {
		base = filepath.Join(base, "azor")
	}
	return filepath.Join(base, "state.json")
}

func main() {
	args := os.Args[1:]
	cmd := "now"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "now":
		err = cmdNow(os.Stdout)
	case "range":
		err = cmdRange(os.Stdout, args)
	case "peaks":
		err = cmdPeaks(os.Stdout, args)
	case "events":
		err = cmdEvents(os.Stdout, args)
	case "status":
		err = cmdStatus(os.Stdout)
	case "brief":
		err = cmdBrief(os.Stdout)
	case "hardware":
		err = cmdHardware(os.Stdout)
	case "health":
		err = cmdHealth(os.Stdout)
	case "timers":
		err = cmdTimers(os.Stdout)
	case "init":
		err = cmdInit(os.Stdout)
	case "files":
		err = cmdFiles(os.Stdout)
	case "config":
		err = cmdConfig(os.Stdout)
	case "daemon":
		err = runDaemon()
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "azor:", err)
		os.Exit(1)
	}
}
