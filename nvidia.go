package main

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// nvidiaReader polls nvidia-smi in the background, because the proprietary
// driver exposes no usable sysfs sensors.
type nvidiaReader struct {
	path, slot string
	mu         sync.Mutex
	vals       map[string]float64
	at         time.Time
	busy       bool
}

func newNvidia(slot string) *nvidiaReader {
	p, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil
	}
	return &nvidiaReader{path: p, slot: slot}
}

// nvidia-smi rejects a whole query when one field is unknown, so a shorter
// list is tried when the full one fails.
var nvidiaFull = [][2]string{{"gpu", "utilization.gpu"}, {"gpu_temp", "temperature.gpu"}, {"gpu_power", "power.draw"}, {"gpu_fan_pct", "fan.speed"},
	{"gpu_mem_clock", "clocks.mem"}, {"gpu_clock", "clocks.gr"}, {"gpu_vram_mb", "memory.used"}, {"gpu_mem_temp", "temperature.memory"}}

var nvidiaShort = nvidiaFull[:3]

func parseNvidia(s string, fields [][2]string) map[string]float64 {
	m := map[string]float64{}
	line, _, _ := strings.Cut(s, "\n")
	f := strings.Split(line, ",")
	if len(f) != len(fields) {
		return m
	}
	for i, x := range fields {
		if v, err := strconv.ParseFloat(strings.TrimSpace(f[i]), 64); err == nil {
			m[x[0]] = v
		}
	}
	return m
}

func (n *nvidiaReader) query(fields [][2]string) map[string]float64 {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var q []string
	for _, f := range fields {
		q = append(q, f[1])
	}
	out, err := exec.CommandContext(ctx, n.path, "-i", n.slot, "--format=csv,noheader,nounits", "--query-gpu="+strings.Join(q, ",")).Output()
	if err != nil {
		return nil
	}
	return parseNvidia(string(out), fields)
}

func (n *nvidiaReader) poll() {
	v := n.query(nvidiaFull)
	if len(v) == 0 {
		v = n.query(nvidiaShort)
	}
	n.mu.Lock()
	n.busy, n.vals = false, v
	n.mu.Unlock()
}

func (n *nvidiaReader) merge(m map[string]float64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for k, v := range n.vals {
		m[k] = v
	}
	if !n.busy && time.Since(n.at) > 9*time.Second {
		n.busy, n.at = true, time.Now()
		go n.poll()
	}
}
