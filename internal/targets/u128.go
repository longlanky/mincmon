package targets

import (
	"encoding/binary"
	"math/bits"
	"net/netip"
)

// u128 is a minimal unsigned 128-bit integer, enough for host-offset
// arithmetic inside IPv6 prefixes.
type u128 struct{ hi, lo uint64 }

func (a u128) add(b u128) (u128, bool) {
	lo, carry := bits.Add64(a.lo, b.lo, 0)
	hi, carry := bits.Add64(a.hi, b.hi, carry)
	return u128{hi, lo}, carry != 0
}

func (a u128) sub(b u128) u128 {
	lo, borrow := bits.Sub64(a.lo, b.lo, 0)
	hi, _ := bits.Sub64(a.hi, b.hi, borrow)
	return u128{hi, lo}
}

func (a u128) cmp(b u128) int {
	switch {
	case a.hi < b.hi:
		return -1
	case a.hi > b.hi:
		return 1
	case a.lo < b.lo:
		return -1
	case a.lo > b.lo:
		return 1
	}
	return 0
}

// mulAdd returns a*m + v, reporting overflow past 128 bits.
func (a u128) mulAdd(m, v uint64) (u128, bool) {
	loHi, lo := bits.Mul64(a.lo, m)
	hiHi, hiLo := bits.Mul64(a.hi, m)
	hi, carry := bits.Add64(hiLo, loHi, 0)
	r, o := u128{hi, lo}.add(u128{0, v})
	return r, hiHi != 0 || carry != 0 || o
}

func addrToU128(a netip.Addr) u128 {
	if a.Is4() {
		b := a.As4()
		return u128{0, uint64(binary.BigEndian.Uint32(b[:]))}
	}
	b := a.As16()
	return u128{binary.BigEndian.Uint64(b[:8]), binary.BigEndian.Uint64(b[8:])}
}

// u128ToAddr converts back to an address of the same family as like.
// ok is false if the value does not fit the family's address space.
func u128ToAddr(v u128, like netip.Addr) (netip.Addr, bool) {
	if like.Is4() {
		if v.hi != 0 || v.lo>>32 != 0 {
			return netip.Addr{}, false
		}
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(v.lo))
		return netip.AddrFrom4(b), true
	}
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], v.hi)
	binary.BigEndian.PutUint64(b[8:], v.lo)
	return netip.AddrFrom16(b), true
}
