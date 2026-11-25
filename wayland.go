package main

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"syscall"
	"time"
)

// output is the current mode of one display as the compositor reports it.
type output struct {
	Name        string
	W, H, MHz   int32
	Scale       int32
	global, ver uint32
}

const maxWayland = 256 << 10 // more input than this is not a sane answer

var ne = binary.NativeEndian

func wlMsg(obj, op uint32, words ...uint32) []byte {
	n := 8 + 4*len(words)
	b := make([]byte, 0, n)
	b = ne.AppendUint32(b, obj)
	b = ne.AppendUint32(b, uint32(n)<<16|op)
	for _, w := range words {
		b = ne.AppendUint32(b, w)
	}
	return b
}

func wlBind(global, version, id uint32) []byte {
	const name = "wl_output\x00\x00\x00" // length 10 with NUL, padded to 12
	b := ne.AppendUint32(nil, 2)
	b = ne.AppendUint32(b, 36<<16)
	b = ne.AppendUint32(b, global)
	b = ne.AppendUint32(b, 10)
	b = append(b, name...)
	b = ne.AppendUint32(b, version)
	return ne.AppendUint32(b, id)
}

func wlString(p []byte) (s string, rest []byte, ok bool) {
	if len(p) < 4 {
		return
	}
	n := int(ne.Uint32(p))
	pad := (n + 3) &^ 3
	if n < 1 || n > 4096 || len(p) < 4+pad {
		return
	}
	return string(p[4 : 4+n-1]), p[4+pad:], true
}

// readOutputs asks a compositor for its display modes. It is a one-shot
// client that binds only wl_output: no window, input, clipboard or screen
// content is reachable through that, and every other interface is ignored.
func readOutputs(path string, uid uint32) ([]output, error) {
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if u, ok := peer(c); !ok || u.Uid != uid {
		return nil, errors.New("socket belongs to another user")
	}
	c.SetDeadline(time.Now().Add(2 * time.Second))
	// get_registry as object 2, sync as object 3
	if _, err := c.Write(append(wlMsg(1, 1, 2), wlMsg(1, 0, 3)...)); err != nil {
		return nil, err
	}
	var outs []output
	byID := map[uint32]int{}
	total, last := 0, uint32(0)
	head := make([]byte, 8)
	for {
		if _, err := io.ReadFull(c, head); err != nil {
			return nil, err
		}
		obj, word := ne.Uint32(head), ne.Uint32(head[4:])
		op, size := word&0xffff, int(word>>16)
		if size < 8 || size%4 != 0 {
			return nil, errors.New("bad message")
		}
		if total += size; total > maxWayland {
			return nil, errors.New("too much data")
		}
		p := make([]byte, size-8)
		if _, err := io.ReadFull(c, p); err != nil {
			return nil, err
		}
		switch {
		case obj == 1 && op == 0:
			return nil, errors.New("protocol error")
		case obj == 2 && op == 0 && len(p) >= 12:
			iface, rest, ok := wlString(p[4:])
			if ok && iface == "wl_output" && len(rest) >= 4 && len(outs) < 32 {
				outs = append(outs, output{global: ne.Uint32(p), ver: min(ne.Uint32(rest), 4)})
			}
		case obj == 2 && op == 1 && len(p) >= 4:
			for i := range outs {
				if outs[i].global == ne.Uint32(p) {
					outs[i].ver = 0
				}
			}
		case obj == 3 && op == 0:
			id := uint32(4)
			var b []byte
			for i := range outs {
				if outs[i].ver == 0 {
					continue
				}
				byID[id] = i
				b = append(b, wlBind(outs[i].global, outs[i].ver, id)...)
				id++
			}
			last = id
			if _, err := c.Write(append(b, wlMsg(1, 0, last)...)); err != nil {
				return nil, err
			}
		case last != 0 && obj == last && op == 0:
			var good []output
			for _, o := range outs {
				if o.ver > 0 && o.W > 0 {
					good = append(good, o)
				}
			}
			return good, nil
		default:
			i, ok := byID[obj]
			if !ok {
				continue
			}
			o := &outs[i]
			switch { // geometry and description are skipped: they identify hardware
			case op == 1 && len(p) >= 16 && ne.Uint32(p)&1 != 0:
				o.W, o.H, o.MHz = int32(ne.Uint32(p[4:])), int32(ne.Uint32(p[8:])), int32(ne.Uint32(p[12:]))
			case op == 3 && len(p) >= 4:
				o.Scale = int32(ne.Uint32(p))
			case op == 4:
				o.Name, _, _ = wlString(p)
			}
		}
	}
}

func peer(c net.Conn) (u syscall.Ucred, ok bool) {
	uc, is := c.(*net.UnixConn)
	if !is {
		return
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return
	}
	raw.Control(func(fd uintptr) {
		if x, e := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED); e == nil {
			u, ok = *x, true
		}
	})
	return
}
