package probe

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const execName = "ping binary"

// execProber shells out to the system ping, one process per probe. It is
// the last-resort fallback when no ICMP socket can be opened.
type execProber struct {
	v6   bool
	bin  string
	args func(addr netip.Addr, timeout time.Duration) []string
}

func newExec(v6 bool) (Prober, error) {
	p := &execProber{v6: v6, bin: "ping"}
	switch runtime.GOOS {
	case "windows":
		p.args = func(a netip.Addr, t time.Duration) []string {
			args := []string{"-n", "1", "-w", strconv.Itoa(int(t.Milliseconds()))}
			if v6 {
				args = append(args, "-6")
			}
			return append(args, a.String())
		}
	case "darwin", "freebsd", "openbsd", "netbsd", "dragonfly":
		if v6 {
			if _, err := exec.LookPath("ping6"); err == nil {
				p.bin = "ping6" // BSD ping6 has no reply-timeout flag
				p.args = func(a netip.Addr, _ time.Duration) []string {
					return []string{"-c", "1", a.String()}
				}
				break
			}
		}
		// BSD/macOS ping -W is in milliseconds
		p.args = func(a netip.Addr, t time.Duration) []string {
			return []string{"-c", "1", "-W", strconv.Itoa(int(t.Milliseconds())), a.String()}
		}
	default:
		if _, err := exec.LookPath("ping6"); v6 && err == nil {
			p.bin = "ping6"
		}
		p.args = func(a netip.Addr, t time.Duration) []string {
			// Linux ping -W is in whole seconds
			return []string{"-c", "1", "-W", strconv.Itoa(max(1, int(t.Seconds()))), a.String()}
		}
	}
	if _, err := exec.LookPath(p.bin); err != nil {
		return nil, fmt.Errorf("%q executable not found", p.bin)
	}
	return p, nil
}

func (p *execProber) Name() string { return execName }
func (p *execProber) Close() error { return nil }

// Unix: "time=22.5 ms"; Windows: "time=22ms" / "time<1ms". Localized output
// won't match, in which case wall-clock time is used.
var latencyRE = regexp.MustCompile(`(?i)time\s*[=<]\s*([0-9]+(?:\.[0-9]+)?)\s*ms`)

func (p *execProber) Probe(ctx context.Context, addr netip.Addr, timeout time.Duration) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout+time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.bin, p.args(addr, timeout)...)
	start := time.Now()
	out, err := cmd.Output()
	elapsed := time.Since(start)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) || ctx.Err() != nil {
			return 0, ErrTimeout
		}
		return 0, err
	}
	m := latencyRE.FindSubmatch(out)
	if m == nil {
		if runtime.GOOS == "windows" && !strings.Contains(strings.ToUpper(string(out)), "TTL=") {
			// Windows ping exits 0 on "Destination host unreachable"
			return 0, ErrTimeout
		}
		return elapsed, nil
	}
	ms, err := strconv.ParseFloat(string(m[1]), 64)
	if err != nil || ms < 0 || ms >= 100000 {
		return elapsed, nil
	}
	return time.Duration(ms * float64(time.Millisecond)), nil
}
