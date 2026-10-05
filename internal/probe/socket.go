package probe

import (
	"context"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// sockProber shares ONE ICMP socket per address family across all
// targets. A receiver goroutine dispatches replies to waiting probes by
// (source address, sequence number), so thousands of hosts cost one socket.
type sockProber struct {
	conn *icmp.PacketConn
	v6   bool
	raw  bool
	id   int
	seq  atomic.Uint32

	mu      sync.Mutex
	pending map[pendingKey]chan time.Time
	closed  atomic.Bool
}

type pendingKey struct {
	addr netip.Addr // zone stripped
	seq  uint16
}

// newSock opens an unprivileged datagram ICMP socket (udp=true) or a raw
// ICMP socket.
func newSock(v6, udp bool) (*sockProber, error) {
	network, laddr := "ip4:icmp", "0.0.0.0"
	switch {
	case v6 && udp:
		network, laddr = "udp6", "::"
	case v6:
		network, laddr = "ip6:ipv6-icmp", "::"
	case udp:
		network = "udp4"
	}
	c, err := icmp.ListenPacket(network, laddr)
	if err != nil {
		return nil, err
	}
	p := &sockProber{
		conn:    c,
		v6:      v6,
		raw:     !udp,
		id:      os.Getpid() & 0xffff,
		pending: map[pendingKey]chan time.Time{},
	}
	go p.receive()
	return p, nil
}

func (p *sockProber) Name() string {
	if p.raw {
		return "raw ICMP socket"
	}
	return "ICMP socket"
}

func (p *sockProber) Close() error {
	p.closed.Store(true)
	return p.conn.Close()
}

func (p *sockProber) Probe(ctx context.Context, addr netip.Addr, timeout time.Duration) (time.Duration, error) {
	seq := uint16(p.seq.Add(1))
	typ := icmp.Type(ipv4.ICMPTypeEcho)
	if p.v6 {
		typ = ipv6.ICMPTypeEchoRequest
	}
	msg := icmp.Message{Type: typ, Body: &icmp.Echo{ID: p.id, Seq: int(seq), Data: payload}}
	wb, err := msg.Marshal(nil) // the kernel fills in the ICMPv6 checksum
	if err != nil {
		return 0, err
	}
	var dst net.Addr = &net.IPAddr{IP: addr.AsSlice(), Zone: addr.Zone()}
	if !p.raw {
		dst = &net.UDPAddr{IP: addr.AsSlice(), Zone: addr.Zone()}
	}

	key := pendingKey{addr.WithZone(""), seq}
	ch := make(chan time.Time, 1)
	p.mu.Lock()
	p.pending[key] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.pending, key)
		p.mu.Unlock()
	}()

	start := time.Now()
	if _, err := p.conn.WriteTo(wb, dst); err != nil {
		return 0, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case recv := <-ch:
		return recv.Sub(start), nil
	case <-timer.C:
		return 0, ErrTimeout
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (p *sockProber) receive() {
	proto := 1 // ICMPv4
	reply := icmp.Type(ipv4.ICMPTypeEchoReply)
	if p.v6 {
		proto, reply = 58, ipv6.ICMPTypeEchoReply
	}
	buf := make([]byte, 1500)
	for {
		n, peer, err := p.conn.ReadFrom(buf)
		now := time.Now()
		if err != nil {
			if p.closed.Load() {
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		b := buf[:n]
		// Defensive: some platforms deliver the IPv4 header on raw/dgram
		// sockets. An echo reply starts with type 0, a header with 0x4_.
		if !p.v6 && len(b) >= 20 && b[0]>>4 == 4 {
			if ihl := int(b[0]&0x0f) * 4; ihl <= len(b) {
				b = b[ihl:]
			}
		}
		m, err := icmp.ParseMessage(proto, b)
		if err != nil || m.Type != reply {
			continue
		}
		echo, ok := m.Body.(*icmp.Echo)
		// Raw sockets see every ICMP packet on the host; datagram sockets
		// only see their own, with the ID rewritten by the kernel.
		if !ok || (p.raw && echo.ID != p.id) {
			continue
		}
		var ip net.IP
		switch a := peer.(type) {
		case *net.UDPAddr:
			ip = a.IP
		case *net.IPAddr:
			ip = a.IP
		}
		src, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		if !p.v6 {
			src = src.Unmap()
		}
		key := pendingKey{src, uint16(echo.Seq)}
		p.mu.Lock()
		ch := p.pending[key]
		delete(p.pending, key)
		p.mu.Unlock()
		if ch != nil {
			ch <- now
		}
	}
}

func newRaw(v6 bool) (Prober, error) {
	p, err := newSock(v6, false)
	if err != nil {
		return nil, err
	}
	return p, nil
}
