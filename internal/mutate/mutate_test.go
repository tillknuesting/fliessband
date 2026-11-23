package mutate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const lib = `package lib

import "log/slog"

// Positive reports whether n is above zero.
func Positive(n int) bool {
	return n > 0
}

// Clamp limits v to [lo, hi]. Its boundary mutants are equivalent: returning
// lo when v == lo returns the same value.
func Clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Greeting is not tested at all.
func Greeting(name string) string {
	if name == "" {
		name = "world"
	}
	slog.Info("greeting", "n", len(name)+1)
	return "hello " + name
}

// Even reports whether n is even; its test checks a single value.
func Even(n int) bool {
	return n%2 == 0 && !false
}
`

const libTest = `package lib

import "testing"

func TestPositive(t *testing.T) {
	if Positive(0) || !Positive(1) {
		t.Error("Positive is wrong")
	}
}

func TestClamp(t *testing.T) {
	for _, c := range []struct{ v, want int }{{-1, 0}, {0, 0}, {5, 5}, {10, 10}, {11, 10}} {
		if got := Clamp(c.v, 0, 10); got != c.want {
			t.Errorf("Clamp(%d) = %d, want %d", c.v, got, c.want)
		}
	}
}

func TestEven(t *testing.T) {
	if !Even(4) {
		t.Error("4 is even")
	}
}
`

func TestFindOperators(t *testing.T) {
	ms, err := Find("lib/lib.go", []byte(lib))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, m := range ms {
		got[m.Operator]++
		if m.Before == "" || m.After == "" || m.Before == m.After {
			t.Errorf("mutant %+v lacks distinct before/after lines", m)
		}
		if strings.Contains(m.Before, "slog.Info") || strings.Contains(m.Before, `"hello " + name`) {
			t.Errorf("mutated a log call or a string concatenation: %+v", m)
		}
	}
	want := map[string]int{"boundary": 3, "negation": 2, "arithmetic": 1, "logical": 1, "not-removal": 1, "bool-literal": 1}
	for op, n := range want {
		if got[op] != n {
			t.Errorf("%s mutants = %d, want %d (all: %v)", op, got[op], n, got)
		}
	}
}

func TestApplyAndSample(t *testing.T) {
	ms, _ := Find("lib/lib.go", []byte(lib))
	for _, m := range ms {
		if m.Operator == "boundary" && m.Original == "<" {
			if out := string(Apply([]byte(lib), m)); !strings.Contains(out, "if v <= lo {") {
				t.Fatalf("apply did not produce <=:\n%s", out)
			}
		}
	}
	sample, sampled := Sample(ms, 4)
	if !sampled || len(sample) != 4 {
		t.Fatalf("sample: %d mutants, sampled %v", len(sample), sampled)
	}
	ops := map[string]bool{}
	for _, m := range sample {
		ops[m.Operator] = true
	}
	if len(ops) < 4 {
		t.Errorf("a sample should spread over operators, got %v", ops)
	}
	if all, sampled := Sample(ms, 100); sampled || len(all) != len(ms) {
		t.Error("a sample under the budget must keep every mutant")
	}
}

func TestRunner(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go test")
	}
	root := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/m\n\ngo 1.25\n")
	write("lib/lib.go", lib)
	write("lib/lib_test.go", libTest)
	cover := exec.Command("go", "test", "-coverprofile=cover.out", "./lib")
	cover.Dir = root
	if out, err := cover.CombinedOutput(); err != nil {
		t.Fatalf("coverage run: %v\n%s", err, out)
	}
	covered, err := Coverage(filepath.Join(root, "cover.out"), "example.com/m")
	if err != nil {
		t.Fatal(err)
	}

	ms, _ := Find("lib/lib.go", []byte(lib))
	for i := range ms {
		ms[i].ID = i + 1
	}
	r := &Runner{Root: root, Env: os.Environ(), Workers: 4, Timeout: 30 * time.Second, Budget: 2 * time.Minute, Covered: covered}
	if _, err := r.Baseline(t.Context(), []string{"./lib"}); err != nil {
		t.Fatal(err)
	}
	if ms, err = r.Run(t.Context(), ms); err != nil {
		t.Fatal(err)
	}
	status := func(before, op string) Status {
		for _, m := range ms {
			if strings.Contains(m.Before, before) && m.Operator == op {
				return m.Status
			}
		}
		t.Fatalf("no %s mutant on %q", op, before)
		return ""
	}
	for _, c := range []struct {
		line, op string
		want     Status
	}{
		{"return n > 0", "boundary", Killed},          // Positive(0) catches >=
		{"if v < lo", "boundary", Survived},           // equivalent mutant
		{"if v > hi", "boundary", Survived},           // equivalent mutant
		{"n%2 == 0 && !false", "logical", Survived},   // || true still holds for 4
		{"n%2 == 0 && !false", "arithmetic", Killed},  // 4*2 != 0
		{"n%2 == 0 && !false", "not-removal", Killed}, // && false
		{`if name == ""`, "negation", NotCovered},     // Greeting has no test
	} {
		if got := status(c.line, c.op); got != c.want {
			t.Errorf("%s mutant on %q: %v, want %v", c.op, c.line, got, c.want)
		}
	}
}
