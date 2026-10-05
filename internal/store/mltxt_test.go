package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	in := "10.0.0.1,a.com\n\n  2001:db8::1 , a.com, b.com \nnot-an-ip,x.com\n10.0.0.2,\n"
	dir := t.TempDir()
	src := filepath.Join(dir, "in"+Ext)
	if err := os.WriteFile(src, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}
	items, err := Load(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[1].Domain != "a.com, b.com" || items[2].Domain != "" {
		t.Fatalf("Load = %+v", items)
	}
	dst := filepath.Join(dir, "out"+Ext)
	if _, err := Save(dst, items); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	want := "10.0.0.1,a.com\n2001:db8::1,a.com, b.com\n10.0.0.2,\n"
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
