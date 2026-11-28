package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Long-term history. Day files are kept for the retention (7 days by
// default). Before one is removed, it is summed up into hourly and daily
// records, and the events worth keeping are copied, so that questions about
// weeks ago still have an answer. Everything stays on disk; nothing is held in
// memory beyond one day file being read.

const (
	historyFile = "history.jsonl"
	keepHours   = 30 // days of hourly records
	keepDays    = 90 // days of daily records and events
)

// longEvents are the events kept beyond the retention.
var longEvents = map[string]bool{"boot": true, "unexpected_boot": true, "interrupted": true, "suspend": true, "gap": true,
	"lost": true, "shutdown": true, "package": true, "alert": true, "clear": true, "timer": true, "unit": true,
	"desktop": true, "health": true, "fact": true}

// span sums samples over one hour or one day.
type span struct {
	end     int64
	secs    float64
	sum, wt map[string]float64
	lo, hi  map[string]float64
}

func newSpan(end int64) *span {
	return &span{end: end, sum: map[string]float64{}, wt: map[string]float64{}, lo: map[string]float64{}, hi: map[string]float64{}}
}

func (s *span) add(r Record, secs float64) {
	s.secs += secs
	for k, v := range r.Avg {
		s.sum[k] += v * secs
		s.wt[k] += secs
		if l, ok := s.lo[k]; !ok || v < l {
			s.lo[k] = v
		}
		top := v
		if m, ok := r.Max[k]; ok && m > top {
			top = m
		}
		if h, ok := s.hi[k]; !ok || top > h {
			s.hi[k] = top
		}
	}
}

func (s *span) record(kind string) Record {
	r := Record{T: s.end, Type: kind, Secs: int64(s.secs), Avg: map[string]float64{}, Min: map[string]float64{}, Max: map[string]float64{}}
	for k, w := range s.wt {
		if w > 0 {
			r.Avg[k], r.Min[k], r.Max[k] = round1(s.sum[k]/w), round1(s.lo[k]), round1(s.hi[k])
		}
	}
	return r
}

// summarize reads one day file and returns its hourly and daily records and
// the events to keep.
func summarize(path string, interval float64) (out []Record, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	hours := map[int64]*span{}
	var day *span
	sp := &spans{fallback: interval}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.V != schema {
			continue
		}
		r.clean()
		switch {
		case r.Type == "sample":
			secs, _ := sp.next(r)
			mid := time.Unix(r.T-int64(secs/2), 0)
			h := mid.Truncate(time.Hour).Add(time.Hour).Unix()
			if hours[h] == nil {
				hours[h] = newSpan(h)
			}
			hours[h].add(r, secs)
			if day == nil {
				y, m, d := mid.Date()
				day = newSpan(time.Date(y, m, d+1, 0, 0, 0, 0, mid.Location()).Unix())
			}
			day.add(r, secs)
		case r.Type == "event" && longEvents[r.Kind]:
			if r.Kind == "gap" || r.Kind == "boot" || r.Kind == "suspend" || r.Kind == "interrupted" || r.Kind == "lost" {
				sp.cut()
			}
			out = append(out, r)
		case r.Type == "event" && r.Kind == "start":
			sp.cut()
		case r.Type == "snapshot":
			out = append(out, r)
		}
	}
	keys := make([]int64, 0, len(hours))
	for h := range hours {
		keys = append(keys, h)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, h := range keys {
		out = append(out, hours[h].record("hour"))
	}
	if day != nil {
		out = append(out, day.record("day"))
	}
	return out, sc.Err()
}

type longState struct {
	Done string `json:"done"` // newest day file already summed up
}

// compact sums up the day files that the retention is about to remove, then
// drops what is older than the long-term limits.
func compact(dir string, now time.Time, retention int, interval float64) error {
	var st longState
	if b, err := os.ReadFile(filepath.Join(dir, "longterm.json")); err == nil {
		json.Unmarshal(b, &st)
	}
	limit := now.AddDate(0, 0, -retention).Format("2006-01-02") + ".jsonl"
	changed := false
	for _, n := range dayFiles(dir) {
		if n >= limit || n <= st.Done {
			continue
		}
		recs, err := summarize(filepath.Join(dir, n), interval)
		if err != nil {
			return err
		}
		if err := appendAll(filepath.Join(dir, historyFile), recs); err != nil {
			return err // the day file stays; it is tried again next time
		}
		st.Done, changed = n, true
		if err := writeAtomic(filepath.Join(dir, "longterm.json"), st); err != nil {
			return err
		}
	}
	if changed {
		return trimHistory(filepath.Join(dir, historyFile), now)
	}
	return nil
}

func appendAll(path string, recs []Record) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, r := range recs {
		r.V = schema
		b, err := json.Marshal(r)
		if err != nil {
			continue
		}
		w.Write(append(b, '\n'))
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// trimHistory rewrites the long-term file without the records past their limit.
func trimHistory(path string, now time.Time) error {
	in, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer in.Close()
	tmp := path + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	hours, rest := now.AddDate(0, 0, -keepHours).Unix(), now.AddDate(0, 0, -keepDays).Unix()
	w := bufio.NewWriter(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var r struct {
			T    int64  `json:"t"`
			Type string `json:"type"`
		}
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		if (r.Type == "hour" || r.Type == "snapshot") && r.T < hours || r.T < rest {
			continue
		}
		w.Write(append(append([]byte{}, sc.Bytes()...), '\n'))
	}
	if err := w.Flush(); err != nil {
		out.Close()
		return err
	}
	out.Close()
	return os.Rename(tmp, path)
}

// loadLong streams the long-term records between from and to.
func loadLong(dir string, from, to time.Time, fn func(Record)) error {
	f, err := os.Open(filepath.Join(dir, historyFile))
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.V != schema || r.T < from.Unix() || r.T > to.Unix() {
			continue
		}
		r.clean()
		fn(r)
	}
	return sc.Err()
}

// rawSince is the start of the oldest day file: before it only the long-term
// history has data.
func rawSince(dir string) time.Time {
	days := dayFiles(dir)
	if len(days) == 0 {
		return time.Now()
	}
	t, err := time.ParseInLocation("2006-01-02", strings.TrimSuffix(days[0], ".jsonl"), time.Local)
	if err != nil {
		return time.Now()
	}
	return t
}

// loadAll gives events and snapshots from both stores without counting any
// twice: the long-term copy is used only before the day files begin.
func loadAll(dir string, from, to time.Time, fn func(Record)) error {
	edge := rawSince(dir)
	if from.Before(edge) {
		if err := loadLong(dir, from, minTime(to, edge.Add(-time.Second)), func(r Record) {
			if r.Type == "event" || r.Type == "snapshot" {
				fn(r)
			}
		}); err != nil {
			return err
		}
	}
	return load(dir, maxTime(from, edge), to, fn)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
