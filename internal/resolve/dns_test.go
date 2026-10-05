package resolve

import (
	"net/netip"
	"testing"
)

// Needs network access; skipped when the lookup returns nothing.
func TestDomain(t *testing.T) {
	cf := []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	got := Domain("one.one.one.one", cf, true, true)
	if len(got) == 0 {
		t.Skip("no DNS access")
	}
	has := map[string]bool{}
	for _, a := range got {
		has[a.String()] = true
	}
	if !has["1.1.1.1"] || !has["2606:4700:4700::1111"] {
		t.Errorf("custom resolver: got %v", got)
	}
	if v4 := Domain("one.one.one.one", cf, true, false); len(v4) == 0 || !v4[0].Is4() {
		t.Errorf("A only: got %v", v4)
	}
	if sys := Domain("one.one.one.one", nil, true, false); len(sys) == 0 {
		t.Errorf("system resolver returned nothing")
	}
}
