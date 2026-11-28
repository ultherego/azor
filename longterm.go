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
	"desktop": true, "health": true, "fact": true, "space": true}

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
func compact(dir string, now time.Time, retention, snapDays int, interval float64) error {
	limit := now.AddDate(0, 0, -retention).Format("2006-01-02") + ".jsonl"
	changed := false
	for _, n := range dayFiles(dir) {
		if n >= limit {
			continue
		}
		done, err := sumUp(dir, n, interval)
		if err != nil {
			return err
		}
		changed = changed || done
	}
	if changed {
		return trimHistory(filepath.Join(dir, historyFile), now, snapDays)
	}
	return nil
}

// sumUp copies one day file into the long-term history, once. It reports
// whether anything was added.
func sumUp(dir, name string, interval float64) (bool, error) {
	var st longState
	if b, err := os.ReadFile(filepath.Join(dir, "longterm.json")); err == nil {
		json.Unmarshal(b, &st)
	}
	if name <= st.Done {
		return false, nil
	}
	recs, err := summarize(filepath.Join(dir, name), interval)
	if err != nil {
		return false, err
	}
	if err := appendAll(filepath.Join(dir, historyFile), recs); err != nil {
		return false, err // the day file stays; it is tried again next time
	}
	st.Done = name
	return true, writeAtomic(filepath.Join(dir, "longterm.json"), st)
}

// diskUse is the size of the day files and of the long-term history.
func diskUse(dir string) (days, long int64) {
	for _, n := range dayFiles(dir) {
		if i, err := os.Stat(filepath.Join(dir, n)); err == nil {
			days += i.Size()
		}
	}
	if i, err := os.Stat(filepath.Join(dir, historyFile)); err == nil {
		long = i.Size()
	}
	return
}

// fitSpace keeps the history within limit bytes. The oldest day files go
// first, each only after it is summed up; today's file always stays. If the
// long-term history is still too large, its oldest records go. Nothing is
// removed silently: each removal is returned as a "space" event.
func fitSpace(dir string, now time.Time, limit int64, interval float64) (out []Record, err error) {
	if limit <= 0 {
		return nil, nil
	}
	today := filepath.Base(dayPath(dir, now))
	for {
		days := dayFiles(dir)
		d, l := diskUse(dir)
		if d+l <= limit || len(days) < 2 || days[0] >= today {
			break
		}
		if _, err := sumUp(dir, days[0], interval); err != nil {
			return out, err
		}
		if err := os.Remove(filepath.Join(dir, days[0])); err != nil {
			return out, err
		}
		out = append(out, Record{T: now.Unix(), Type: "event", Kind: "space", Key: strings.TrimSuffix(days[0], ".jsonl"), New: "day"})
	}
	d, l := diskUse(dir)
	if d+l <= limit || d >= limit { // with only today's file over the limit, the history is kept
		return out, nil
	}
	cut, err := trimToSize(filepath.Join(dir, historyFile), limit-d)
	if err != nil {
		return out, err
	}
	if cut > 0 {
		out = append(out, Record{T: now.Unix(), Type: "event", Kind: "space", Key: time.Unix(cut, 0).Format("2006-01-02 15:04"), New: "history"})
	}
	return out, nil
}

// trimToSize drops the oldest long-term records until the file fits in room
// bytes. It returns the time of the oldest record kept, or 0 if nothing went.
func trimToSize(path string, room int64) (int64, error) {
	type line struct{ t, n int64 }
	var lines []line
	var total int64
	err := scanLong(path, func(t int64, b []byte) {
		lines = append(lines, line{t, int64(len(b)) + 1})
		total += int64(len(b)) + 1
	})
	if err != nil || total <= room {
		return 0, err
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].t > lines[j].t })
	var used, cut int64
	for _, x := range lines {
		if used+x.n > room {
			break
		}
		used += x.n
		cut = x.t
	}
	if cut == 0 { // not even one record fits
		cut = lines[0].t + 1
	}
	in, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	tmp := path + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	w := bufio.NewWriter(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var r struct {
			T int64 `json:"t"`
		}
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.T >= cut {
			w.Write(append(append([]byte{}, sc.Bytes()...), '\n'))
		}
	}
	if err := w.Flush(); err != nil {
		out.Close()
		return 0, err
	}
	out.Close()
	if err := sc.Err(); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return cut, os.Rename(tmp, path)
}

// scanLong gives the time and the raw line of each long-term record.
func scanLong(path string, fn func(int64, []byte)) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var r struct {
			T int64 `json:"t"`
		}
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			fn(r.T, sc.Bytes())
		}
	}
	return sc.Err()
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
func trimHistory(path string, now time.Time, snapDays int) error {
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
	snaps := now.AddDate(0, 0, -snapDays).Unix()
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
		if r.Type == "hour" && r.T < hours || r.Type == "snapshot" && r.T < snaps || r.T < rest {
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
