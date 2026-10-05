//go:build !windows

package probe

// newNative opens an unprivileged ICMP datagram socket. On Linux this needs
// the process's group inside net.ipv4.ping_group_range; macOS allows it
// by default.
func newNative(v6 bool) (Prober, error) {
	p, err := newSock(v6, true)
	if err != nil {
		return nil, err
	}
	return p, nil
}
