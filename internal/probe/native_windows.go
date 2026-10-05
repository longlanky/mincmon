//go:build windows

package probe

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows ICMP helper API (iphlpapi.dll). It needs no administrator
// rights, unlike raw sockets.
var (
	iphlpapi            = windows.NewLazySystemDLL("iphlpapi.dll")
	procIcmpCreateFile  = iphlpapi.NewProc("IcmpCreateFile")
	procIcmp6CreateFile = iphlpapi.NewProc("Icmp6CreateFile")
	procIcmpCloseHandle = iphlpapi.NewProc("IcmpCloseHandle")
	procIcmpSendEcho    = iphlpapi.NewProc("IcmpSendEcho")
	procIcmp6SendEcho2  = iphlpapi.NewProc("Icmp6SendEcho2")
)

type ipOptionInformation struct {
	Ttl, Tos, Flags, OptionsSize uint8
	OptionsData                  uintptr
}

// Reply buffer offsets. ICMP_ECHO_REPLY: Address(4) Status(4) RTT(4) ...
// ICMPV6_ECHO_REPLY: IPV6_ADDRESS_EX (26 bytes, packed) + 2 pad, then
// Status(4) and RoundTripTime(4).
const (
	v4StatusOff, v4RTTOff = 4, 8
	v6StatusOff, v6RTTOff = 28, 32
)

type winProber struct{ v6 bool }

func newNative(v6 bool) (Prober, error) {
	h, err := createHandle(v6)
	if err != nil {
		return nil, err
	}
	procIcmpCloseHandle.Call(h)
	return &winProber{v6: v6}, nil
}

func createHandle(v6 bool) (uintptr, error) {
	proc := procIcmpCreateFile
	if v6 {
		proc = procIcmp6CreateFile
	}
	if err := proc.Find(); err != nil {
		return 0, err
	}
	h, _, err := proc.Call()
	if windows.Handle(h) == windows.InvalidHandle {
		return 0, fmt.Errorf("%s: %v", proc.Name, err)
	}
	return h, nil
}

func (p *winProber) Name() string { return "Windows ICMP API" }
func (p *winProber) Close() error { return nil }

func (p *winProber) Probe(ctx context.Context, addr netip.Addr, timeout time.Duration) (time.Duration, error) {
	// One handle per call: the API is synchronous and this avoids any
	// question of sharing a handle across concurrent callers.
	h, err := createHandle(p.v6)
	if err != nil {
		return 0, err
	}
	defer procIcmpCloseHandle.Call(h)

	reply := make([]byte, 256+len(payload))
	opts := ipOptionInformation{Ttl: 128}
	ms := uintptr(timeout.Milliseconds())
	start := time.Now()
	var n uintptr
	var callErr error
	if !p.v6 {
		a4 := addr.As4()
		n, _, callErr = procIcmpSendEcho.Call(h,
			uintptr(binary.LittleEndian.Uint32(a4[:])), // IPAddr is network order in memory
			uintptr(unsafe.Pointer(&payload[0])), uintptr(len(payload)),
			uintptr(unsafe.Pointer(&opts)),
			uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)), ms)
	} else {
		src := windows.RawSockaddrInet6{Family: windows.AF_INET6}
		dst := windows.RawSockaddrInet6{Family: windows.AF_INET6, Addr: addr.As16(), Scope_id: scopeID(addr.Zone())}
		n, _, callErr = procIcmp6SendEcho2.Call(h, 0, 0, 0,
			uintptr(unsafe.Pointer(&src)), uintptr(unsafe.Pointer(&dst)),
			uintptr(unsafe.Pointer(&payload[0])), uintptr(len(payload)),
			uintptr(unsafe.Pointer(&opts)),
			uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)), ms)
	}
	elapsed := time.Since(start)
	if n == 0 {
		// IP_STATUS codes (11000-11999: timed out, unreachable, ...) mean
		// "no reply"; anything else is a real error.
		if errno, ok := callErr.(windows.Errno); ok && (errno < 11000 || errno > 11999) {
			return 0, callErr
		}
		return 0, ErrTimeout
	}
	statusOff, rttOff := v4StatusOff, v4RTTOff
	if p.v6 {
		statusOff, rttOff = v6StatusOff, v6RTTOff
	}
	if binary.LittleEndian.Uint32(reply[statusOff:]) != 0 { // != IP_SUCCESS
		return 0, ErrTimeout
	}
	rtt := time.Duration(binary.LittleEndian.Uint32(reply[rttOff:])) * time.Millisecond
	if rtt == 0 {
		rtt = elapsed // the API reports whole milliseconds; keep sub-ms detail
	}
	return rtt, nil
}

func scopeID(zone string) uint32 {
	if zone == "" {
		return 0
	}
	if n, err := strconv.ParseUint(zone, 10, 32); err == nil {
		return uint32(n)
	}
	if ifi, err := net.InterfaceByName(zone); err == nil {
		return uint32(ifi.Index)
	}
	return 0
}
