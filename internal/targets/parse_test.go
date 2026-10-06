package targets

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		in     string
		kind   Kind
		single bool
		prefix string
	}{
		{"10.0.0.1", Network, true, "10.0.0.1/32"},
		{" 10.0.0.5/24 ", Network, false, "10.0.0.0/24"},
		{"10.0.0.7/32", Network, true, "10.0.0.7/32"},
		{"10.1.2.3/255.255.0.0", Network, false, "10.1.0.0/16"},
		{"2001:db8::1", Network, true, "2001:db8::1/128"},
		{"2001:db8::1/64", Network, false, "2001:db8::/64"},
		{"example.com", Domain, false, ""},
		{"10.0.0.0/255.0.255.0", Domain, false, ""}, // non-contiguous mask
	}
	for _, c := range cases {
		e := Classify(c.in)
		if e.Kind != c.kind || e.Single != c.single {
			t.Errorf("Classify(%q) = %+v", c.in, e)
			continue
		}
		if c.kind == Network && e.Prefix.String() != c.prefix {
			t.Errorf("Classify(%q) prefix = %s, want %s", c.in, e.Prefix, c.prefix)
		}
	}
}

func TestParseEntries(t *testing.T) {
	es := ParseEntries(" 1.1.1.1, ,example.com,,10.0.0.0/30 ")
	if len(es) != 3 || es[1].Name != "example.com" || es[2].Prefix.Bits() != 30 {
		t.Fatalf("ParseEntries = %+v", es)
	}
}

// Expected values below were generated from the original Python implementation's expand_network.
func TestExpand(t *testing.T) {
	cases := []struct {
		net, refine string
		first, last []string
		n           int
		warned      bool
	}{
		{"10.0.0.0/30", "", []string{"10.0.0.1", "10.0.0.2"}, []string{"10.0.0.1", "10.0.0.2"}, 2, false},
		{"10.0.0.0/31", "", []string{"10.0.0.0", "10.0.0.1"}, []string{"10.0.0.0", "10.0.0.1"}, 2, false},
		{"10.0.0.5/24", "1,5-8,300", []string{"10.0.0.1", "10.0.0.5", "10.0.0.6"}, []string{"10.0.0.7", "10.0.0.8"}, 5, true}, // 300 outside /24: skipped with warn
		{"2001:db8::/126", "", []string{"2001:db8::1", "2001:db8::2", "2001:db8::3"}, []string{"2001:db8::2", "2001:db8::3"}, 3, false},
		{"2001:db8::/127", "", []string{"2001:db8::", "2001:db8::1"}, []string{"2001:db8::", "2001:db8::1"}, 2, false},
		{"2001:db8::/64", "1,a-f,1:0", []string{"2001:db8::1", "2001:db8::a", "2001:db8::b"}, []string{"2001:db8::f", "2001:db8::1:0"}, 8, false},
		{"10.0.0.0/16", "", []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}, []string{"10.0.3.255", "10.0.4.0"}, 1024, true},
		{"2001:db8::/120", "ff-0", []string{"2001:db8::", "2001:db8::1", "2001:db8::2"}, []string{"2001:db8::fe", "2001:db8::ff"}, 256, false},
		{"::/0", "", []string{"::1", "::2", "::3"}, []string{"::3ff", "::400"}, 1024, true},
		{"0.0.0.0/0", "4294967296", nil, nil, 0, true}, // past the address space: skipped with warn
		{"255.255.255.252/30", "", []string{"255.255.255.253", "255.255.255.254"}, []string{"255.255.255.253", "255.255.255.254"}, 2, false},
	}
	for _, c := range cases {
		e := Classify(c.net)
		v6 := e.Prefix.Addr().Is6()
		offs, refined, err := ParseRefine(c.refine, v6, HostCap)
		if err != nil {
			t.Fatalf("%s: ParseRefine(%q): %v", c.net, c.refine, err)
		}
		warned := false
		got := Expand(e, offs, refined, HostCap, func(string) { warned = true })
		if len(got) != c.n || warned != c.warned {
			t.Errorf("%s %q: got %d hosts (warned=%v), want %d (warned=%v)", c.net, c.refine, len(got), warned, c.n, c.warned)
			continue
		}
		if c.n == 0 {
			continue
		}
		strs := func(as []netip.Addr) []string {
			var s []string
			for _, a := range as {
				s = append(s, a.String())
			}
			return s
		}
		if f := strs(got[:min(3, len(got))]); !reflect.DeepEqual(f, c.first) {
			t.Errorf("%s %q: first = %v, want %v", c.net, c.refine, f, c.first)
		}
		if l := strs(got[len(got)-2:]); !reflect.DeepEqual(l, c.last) {
			t.Errorf("%s %q: last = %v, want %v", c.net, c.refine, l, c.last)
		}
	}
}

func TestParseRefineErrors(t *testing.T) {
	cases := map[string]string{
		"0-ffffffff":                        "more than 1024",
		"x":                                 "bad offset",
		"1,2-":                              "bad offset",
		"1:2:3:4:5:6:7:8:9":                 "bad offset",
		"10000:0":                           "bad offset",
		"ffffffffffffffffffffffffffffffff0": "bad offset",
	}
	for in, want := range cases {
		_, _, err := ParseRefine(in, true, HostCap)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseRefine(%q) err = %v, want %q", in, err, want)
		}
	}
	if _, _, err := ParseRefine("1-1024", false, HostCap); err != nil {
		t.Errorf("exactly cap hosts should be allowed: %v", err)
	}
	if _, _, err := ParseRefine("1-1024,5", false, HostCap); err == nil {
		t.Errorf("cap+1 hosts should fail")
	}
	if _, refined, _ := ParseRefine("  ", false, HostCap); refined {
		t.Errorf("blank input should mean no refinement")
	}
	o, _, err := ParseRefine("0x10,1:0", true, HostCap)
	if err != nil || o[0] != (Offset{0, 16}) || o[1] != (Offset{0, 65536}) {
		t.Errorf("hex offsets = %v, %v", o, err)
	}
}

func TestHostCount(t *testing.T) {
	for in, want := range map[string]int{
		"10.0.0.0/24": 256, "10.0.0.0/23": 512, "10.0.0.0/8": 1024, "::/0": 1024, "10.0.0.1/32": 1,
	} {
		if got := HostCount(netip.MustParsePrefix(in), HostCap); got != want {
			t.Errorf("HostCount(%s) = %d, want %d", in, got, want)
		}
	}
}

func TestDedupe(t *testing.T) {
	a, b := netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("2.2.2.2")
	got := Dedupe([]Target{{a, "a.com"}, {a, "b.com"}, {b, ""}, {b, "c.com"}, {a, "a.com"}})
	want := []Target{{a, "a.com, b.com"}, {b, "c.com"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Dedupe = %v, want %v", got, want)
	}
}
