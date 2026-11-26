package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
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
	Desktop  []proc             `json:"desktop,omitempty"`
	Services []proc             `json:"services,omitempty"`
	System   []proc             `json:"system,omitempty"`
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
	out, _ := listDays(dir)
	return out
}

// listDays reports a directory that exists but cannot be listed.
func listDays(dir string) ([]string, error) {
	es, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	var out []string
	for _, e := range es {
		if e.Type().IsRegular() && dayFile.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
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
	days, err := listDays(dir)
	if err != nil {
		return fmt.Errorf("history unreadable: %w", err)
	}
	for _, n := range days {
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

const factsFile = "facts.json"

// saveFacts keeps the last facts apart from the day files, which expire.
func saveFacts(dir string, f map[string]string) {
	if len(f) > 0 {
		writeAtomic(filepath.Join(dir, factsFile), f)
	}
}

func lastFacts(dir string, now time.Time, days int) (f map[string]string) {
	if b, err := os.ReadFile(filepath.Join(dir, factsFile)); err == nil && json.Unmarshal(b, &f) == nil && len(f) > 0 {
		return f
	}
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

// lastSample reads only the newest day files, from the end.
func lastSample(dir string, now time.Time) (last int64) {
	fs := dayFiles(dir)
	for i := len(fs) - 1; i >= 0 && i >= len(fs)-2 && last == 0; i-- {
		f, err := os.Open(filepath.Join(dir, fs[i]))
		if err != nil {
			continue
		}
		size, _ := f.Seek(0, io.SeekEnd)
		f.Seek(max(0, size-256<<10), io.SeekStart)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			var r Record
			if json.Unmarshal(sc.Bytes(), &r) == nil && r.Type == "sample" && r.T <= now.Unix() {
				last = max(last, r.T)
			}
		}
		f.Close()
	}
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
