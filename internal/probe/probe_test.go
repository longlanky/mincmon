package probe

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"
)

func TestBackends(t *testing.T) {
	for _, mode := range []Mode{ICMP, Raw, Exec} {
		t.Run(string(mode), func(t *testing.T) {
			p, _, err := New(mode)
			if err != nil {
				t.Skipf("%s backend unavailable: %v", mode, err)
			}
			defer p.Close()
			ctx := context.Background()
			for _, s := range []string{"127.0.0.1", "::1"} {
				rtt, err := p.Probe(ctx, netip.MustParseAddr(s), time.Second)
				if err != nil {
					t.Errorf("%s via %s: %v", s, p.Name(), err)
				} else if rtt <= 0 || rtt > time.Second {
					t.Errorf("%s via %s: implausible rtt %v", s, p.Name(), rtt)
				}
			}
			// TEST-NET-1 is never routed: expect a timeout, not a hang
			start := time.Now()
			_, err = p.Probe(ctx, netip.MustParseAddr("192.0.2.1"), 300*time.Millisecond)
			if !errors.Is(err, ErrTimeout) && err == nil {
				t.Errorf("192.0.2.1 answered?")
			}
			if time.Since(start) > 2*time.Second {
				t.Errorf("timeout took %v", time.Since(start))
			}
		})
	}
}

// Many concurrent probes over the shared socket must each get their own
// reply.
func TestConcurrentShared(t *testing.T) {
	p, _, err := New(ICMP)
	if err != nil {
		t.Skip(err)
	}
	defer p.Close()
	var wg sync.WaitGroup
	var mu sync.Mutex
	fails := 0
	for i := 1; i <= 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := netip.AddrFrom4([4]byte{127, 0, byte(i / 250), byte(i%250 + 1)})
			if _, err := p.Probe(context.Background(), a, 2*time.Second); err != nil {
				mu.Lock()
				fails++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if fails > 0 {
		t.Errorf("%d/200 loopback probes failed", fails)
	}
}
