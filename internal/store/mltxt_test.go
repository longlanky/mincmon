package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	in := "10.0.0.1,a.com\n\n  2001:db8::1 , a.com, b.com \nnot-an-ip,x.com\n10.0.0.2,\n" +
		"10.0.0.3,corp.example.com,10.0.0.53\n" +
		"10.0.0.4,a.com, b.com,10.0.0.53\n" // resolver after merged labels
	dir := t.TempDir()
	src := filepath.Join(dir, "in"+Ext)
	if err := os.WriteFile(src, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}
	items, err := Load(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 5 ||
		items[1].Domain != "a.com, b.com" || items[2].Domain != "" ||
		items[3].Domain != "corp.example.com" || !items[3].Resolver.IsValid() ||
		items[4].Domain != "a.com, b.com" {
		t.Fatalf("Load = %+v", items)
	}
	if items[3].Resolver.String() != "10.0.0.53" || items[4].Resolver.String() != "10.0.0.53" {
		t.Fatalf("resolver provenance = %v / %v", items[3].Resolver, items[4].Resolver)
	}
	if items[0].Resolver.IsValid() || items[1].Resolver.IsValid() {
		t.Fatalf("system-resolved rows must keep zero resolver: %+v %+v", items[0], items[1])
	}
	dst := filepath.Join(dir, "out"+Ext)
	if _, err := Save(dst, items); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	want := "10.0.0.1,a.com\n2001:db8::1,a.com, b.com\n10.0.0.2,\n" +
		"10.0.0.3,corp.example.com,10.0.0.53\n10.0.0.4,a.com, b.com,10.0.0.53\n"
	if string(got) != want {
		t.Errorf("Save wrote %q, want %q", got, want)
	}
}

// The real saved list in the repo root must load and save back unchanged.
func TestInfraList(t *testing.T) {
	path := filepath.Join("..", "..", "infra.ml.txt")
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Skip("infra.ml.txt not present")
	}
	items, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "x"+Ext)
	if _, err := Save(dst, items); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != string(orig) {
		t.Errorf("round trip of infra.ml.txt changed contents:\n%s\n---\n%s", orig, got)
	}
}
