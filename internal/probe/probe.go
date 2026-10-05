// Package probe sends ICMP echo requests. Backends, in preference order:
// unprivileged ICMP datagram sockets (Linux/macOS), the Windows ICMP API,
// raw ICMP sockets (root / CAP_NET_RAW), and finally the system ping
// binary.
package probe

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"runtime"
	"time"
)

// ErrTimeout means no reply arrived within the timeout.
var ErrTimeout = errors.New("timeout")

// Prober sends one echo request and waits for the reply.
type Prober interface {
	// Probe returns the round-trip time, or an error (ErrTimeout if the
	// host simply did not answer).
	Probe(ctx context.Context, addr netip.Addr, timeout time.Duration) (time.Duration, error)
	Name() string
	Close() error
}

// Mode selects which backend(s) New may use.
type Mode string

const (
	Auto Mode = "auto"
	ICMP Mode = "icmp" // unprivileged sockets, or the Windows ICMP API
	Raw  Mode = "raw"
	Exec Mode = "exec"
)

// New picks a backend per address family. In Auto mode it falls back
// through ICMP → raw → exec; hint is non-empty if it had to fall back to
// exec on a platform where native ICMP could have been enabled.
func New(mode Mode) (p Prober, hint string, err error) {
	v4, err4 := pick(mode, false)
	v6, err6 := pick(mode, true)
	if err4 != nil && err6 != nil {
		return nil, "", fmt.Errorf("no usable %s backend: %v", mode, err4)
	}
	if mode == Auto && runtime.GOOS == "linux" && v4 != nil && v4.Name() == execName {
		hint = "native ICMP unavailable, using the ping binary. Enable it with " +
			"`sysctl -w net.ipv4.ping_group_range='0 2147483647'` or " +
			"`setcap cap_net_raw+ep <mincmon binary>`"
	}
	return &dual{v4: v4, v6: v6, err4: err4, err6: err6}, hint, nil
}

func pick(mode Mode, v6 bool) (Prober, error) {
	var tries []func(bool) (Prober, error)
	switch mode {
	case ICMP:
		tries = []func(bool) (Prober, error){newNative}
	case Raw:
		tries = []func(bool) (Prober, error){newRaw}
	case Exec:
		tries = []func(bool) (Prober, error){newExec}
	default:
		tries = []func(bool) (Prober, error){newNative, newRaw, newExec}
	}
	var lastErr error
	for _, f := range tries {
		p, err := f(v6)
		if err == nil {
			return p, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// dual routes probes to a per-family backend.
type dual struct {
	v4, v6     Prober
	err4, err6 error
}

func (d *dual) Probe(ctx context.Context, addr netip.Addr, timeout time.Duration) (time.Duration, error) {
	p, err := d.v4, d.err4
	if addr.Is6() && !addr.Is4In6() {
		p, err = d.v6, d.err6
	}
	if p == nil {
		return 0, err
	}
	return p.Probe(ctx, addr.Unmap(), timeout)
}

func (d *dual) Name() string {
	switch {
	case d.v4 == nil:
		return "IPv6: " + d.v6.Name()
	case d.v6 == nil:
		return "IPv4: " + d.v4.Name()
	case d.v4.Name() == d.v6.Name():
		return d.v4.Name()
	}
	return "IPv4: " + d.v4.Name() + ", IPv6: " + d.v6.Name()
}

func (d *dual) Close() error {
	for _, p := range []Prober{d.v4, d.v6} {
		if p != nil {
			p.Close()
		}
	}
	return nil
}

// payload is the echo request body.
var payload = []byte("mincmon-echo-req")
