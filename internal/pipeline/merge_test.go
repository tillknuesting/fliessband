package pipeline

import (
	"strings"
	"testing"
)

func TestTestMerger(t *testing.T) {
	const impl = "package service\n\nconst maxIDLen = 200\n"
	const winner = `package service

import "testing"

func TestA(t *testing.T) {
	if 1+1 != 2 {
		t.Fatal("math")
	}
}
`
	const loser = `package service

import (
	"strings"
	"testing"
)

const maxIDLen = 100 // clashes with the winner's code

func long(n int) string { return strings.Repeat("a", n) }

// TestA has the winner's name but other content.
func TestA(t *testing.T) {
	if long(2) != "aa" {
		t.Fatal("long")
	}
}

func TestSame(t *testing.T) {
	if 1+1 != 2 {
		t.Fatal("math")
	}
}

func TestClash(t *testing.T) {
	if maxIDLen != 100 {
		t.Fatal("clash")
	}
}

func TestFailed(t *testing.T) { t.Fatal("never merged: it did not pass") }
`
	m, err := newTestMerger(impl, winner)
	if err != nil {
		t.Fatal(err)
	}
	// TestSame is the winner's TestA under another name: dropped.
	skipped := m.add("vendor/other-1", loser, []string{"TestA", "TestSame", "TestClash"})
	if len(skipped) != 1 || !strings.Contains(skipped[0], "maxIDLen") {
		t.Errorf("skipped = %v, want TestClash left out for its helper", skipped)
	}
	// The same file again adds nothing; a changed TestA gets a numbered name.
	m.add("vendor/other-1", loser, []string{"TestA"})
	m.add("vendor/other-1", strings.Replace(loser, `long(2) != "aa"`, `long(3) != "aaa"`, 1), []string{"TestA"})
	if len(m.merged) != 2 || m.merged[0] != "vendor/other-1: TestA as TestAFromVendorOther1" || m.merged[1] != "vendor/other-1: TestA as TestAFromVendorOther12" {
		t.Errorf("merged = %v", m.merged)
	}
	src, err := m.source()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\"strings\"", "func long(n int) string", "func TestAFromVendorOther1(t *testing.T)", "// TestA has the winner's name"} {
		if !strings.Contains(src, want) {
			t.Errorf("merged file lacks %q:\n%s", want, src)
		}
	}
	if strings.Count(src, "func long(") != 1 || strings.Contains(src, "TestFailed") || strings.Contains(src, "TestClash") ||
		strings.Contains(src, "TestSame") || strings.Contains(src, "= 100") {
		t.Errorf("merged file has duplicates or tests that should be left out:\n%s", src)
	}
	if names := testNames(src); len(names) != 3 {
		t.Errorf("tests in the merged file = %v", names)
	}
}
