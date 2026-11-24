package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tillknuesting/fliessband/internal/mutate"
)

func write(t *testing.T, root, rel, src string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A package's key changes when it or anything its tests link changes, and
// only then.
func TestPackageKey(t *testing.T) {
	root := t.TempDir()
	write(t, root, "go.mod", "module example.com/m\n\ngo 1.25\n")
	write(t, root, "a/a.go", "package a\n\nfunc A() int { return 1 }\n")
	write(t, root, "b/b.go", "package b\n\nimport \"example.com/m/a\"\n\nfunc B() int { return a.A() }\n")
	write(t, root, "b/b_test.go", "package b\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) { _ = B() }\n")
	write(t, root, "c/c.go", "package c\n\nfunc C() {}\n")
	env := append(os.Environ(), "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local")
	key := func(dir string) string {
		k, err := packageKey(t.Context(), env, root, dir, "salt")
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	b1, c1 := key("b"), key("c")
	write(t, root, "c/c.go", "package c\n\nfunc C() { _ = 1 }\n")
	if key("b") != b1 {
		t.Error("b's key changed when an unrelated package changed")
	}
	if key("c") == c1 {
		t.Error("c's key did not change when c changed")
	}
	write(t, root, "a/a.go", "package a\n\nfunc A() int { return 2 }\n")
	if key("b") == b1 {
		t.Error("b's key did not change when its dependency changed")
	}
	b2 := key("b")
	write(t, root, "b/b_test.go", "package b\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) { if B() != 2 { t.Fatal() } }\n")
	if key("b") == b2 {
		t.Error("b's key did not change when its tests changed")
	}
}

func TestMutantCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := mutate.Mutant{File: "internal/store/memory.go", Line: 10, Col: 4, Operator: "conditional", Original: "<", Mutated: "<="}
	c := &mutantCache{dir: dir, pkgKeys: map[string]string{"internal/store": "k1"}, entries: map[string]map[string]mutate.Status{}, dirty: map[string]bool{}}
	if _, ok := c.Get(m); ok {
		t.Fatal("empty cache answered")
	}
	c.Put(m, mutate.Killed)
	c.flush()
	fresh := &mutantCache{dir: dir, pkgKeys: map[string]string{"internal/store": "k1"}, entries: map[string]map[string]mutate.Status{}, dirty: map[string]bool{}}
	if st, ok := fresh.Get(m); !ok || st != mutate.Killed {
		t.Fatalf("after flush got %v %v, want killed", st, ok)
	}
	other := &mutantCache{dir: dir, pkgKeys: map[string]string{"internal/store": "k2"}, entries: map[string]map[string]mutate.Status{}, dirty: map[string]bool{}}
	if _, ok := other.Get(m); ok {
		t.Error("a different package key answered")
	}
	m.Mutated = ">"
	if _, ok := fresh.Get(m); ok {
		t.Error("a different mutant answered")
	}
}
