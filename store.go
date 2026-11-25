package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

const schema = 1

// Record is one JSONL line: a sample, a process list or an event.
type Record struct {
	V        int                `json:"v"`
	T        int64              `json:"t"`
	Type     string             `json:"type"`
	Avg      map[string]float64 `json:"avg,omitempty"`
	Max      map[string]float64 `json:"max,omitempty"`
	CPU      []proc             `json:"top_cpu,omitempty"`
	Mem      []proc             `json:"top_mem,omitempty"`
	Watch    []proc             `json:"watch,omitempty"`
	Services []proc             `json:"services,omitempty"`
	Facts    map[string]string  `json:"facts,omitempty"`
	Old      string             `json:"old,omitempty"`
	New      string             `json:"new,omitempty"`
	Kind     string             `json:"kind,omitempty"` // event: alert, clear, gap, start
	Key      string             `json:"key,omitempty"`
	Val      float64            `json:"val,omitempty"`
	Secs     int64              `json:"secs,omitempty"`
}

var dayFile = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.jsonl$`)

func dayPath(dir string, t time.Time) string {
	return filepath.Join(dir, t.Format("2006-01-02")+".jsonl")
}

func appendRecord(dir string, r Record) error {
	r.V = schema
	r.clean()
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(dayPath(dir, time.Unix(r.T, 0)), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if e := f.Close(); err == nil {
		err = e
	}
	return err
}

// dayFiles lists only files Azor created itself, oldest first.
func dayFiles(dir string) []string {
	es, _ := os.ReadDir(dir)
	var out []string
	for _, e := range es {
		if e.Type().IsRegular() && dayFile.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// prune removes day files older than the retention; nothing else is touched.
func prune(dir string, now time.Time, days int) {
	limit := now.AddDate(0, 0, -days).Format("2006-01-02") + ".jsonl"
	for _, n := range dayFiles(dir) {
		if n < limit {
			os.Remove(filepath.Join(dir, n))
		}
	}
}

// load streams records between from and to; damaged lines (for example after
// a power cut) and newer schemas are skipped.
func load(dir string, from, to time.Time, fn func(Record)) error {
	lo, hi := from.Format("2006-01-02")+".jsonl", to.Format("2006-01-02")+".jsonl"
	for _, n := range dayFiles(dir) {
		if n < lo || n > hi {
			continue
		}
		f, err := os.Open(filepath.Join(dir, n))
		if err != nil {
			return err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var r Record
			if json.Unmarshal(sc.Bytes(), &r) != nil || r.V != schema || r.T < from.Unix() || r.T > to.Unix() {
				continue
			}
			r.clean() // the file may have been edited
			fn(r)
		}
		err = sc.Err()
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func lastFacts(dir string, now time.Time, days int) (f map[string]string) {
	load(dir, now.AddDate(0, 0, -days), now, func(r Record) {
		switch {
		case r.Type == "facts":
			f = r.Facts
		case r.Type == "event" && r.Kind == "fact" && f != nil:
			if r.New == "" {
				delete(f, r.Key)
			} else {
				f[r.Key] = r.New
			}
		}
	})
	return
}

func writeAtomic(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
