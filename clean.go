package main

import (
	"strings"
	"unicode"
)

// Names of processes, files, services and devices are chosen by other
// programs, so they are untrusted: control and invisible formatting
// characters could forge terminal output or instructions to a reader.
func clean(s string) string { return cleanTo(s, 240) }

func cleanTo(s string, limit int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == limit {
			break
		}
		if r == unicode.ReplacementChar || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ' {
			r = '?'
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

func cleanProcs(ps []proc) {
	for i := range ps {
		ps[i].Name = cleanTo(ps[i].Name, 80)
	}
}

func cleanKeys[T any](m map[string]T) map[string]T {
	if m == nil {
		return nil
	}
	out := make(map[string]T, len(m))
	for k, v := range m {
		out[clean(k)] = v
	}
	return out
}

func cleanFacts(m map[string]string) map[string]string {
	out := cleanKeys(m)
	for k, v := range out {
		out[k] = clean(v)
	}
	return out
}

func (r *Record) clean() {
	r.Kind, r.Key, r.Old, r.New = clean(r.Kind), clean(r.Key), clean(r.Old), clean(r.New)
	for _, l := range [][]proc{r.CPU, r.Mem, r.Watch, r.Services, r.Desktop, r.System} {
		cleanProcs(l)
	}
	r.Avg, r.Max, r.Min, r.Facts = cleanKeys(r.Avg), cleanKeys(r.Max), cleanKeys(r.Min), cleanFacts(r.Facts)
}

func (s *State) clean() {
	for _, l := range [][]proc{s.CPU, s.Mem, s.Watch, s.Services, s.Desktop, s.System} {
		cleanProcs(l)
	}
	for i := range s.Failed {
		s.Failed[i] = clean(s.Failed[i])
	}
	s.WriteError = clean(s.WriteError)
	for i := range s.Unknown {
		s.Unknown[i] = clean(s.Unknown[i])
	}
	for i := range s.Problems {
		s.Problems[i] = clean(s.Problems[i])
	}
	for i := range s.Timers {
		s.Timers[i].Name, s.Timers[i].Result = clean(s.Timers[i].Name), clean(s.Timers[i].Result)
	}
	for i := range s.Alerts {
		s.Alerts[i] = clean(s.Alerts[i])
	}
	s.Values, s.FS, s.Facts = cleanKeys(s.Values), cleanKeys(s.FS), cleanFacts(s.Facts)
}
