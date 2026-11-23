package metrics

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

const sample = `package p

func sumOfPrimes(max int) int {
	total := 0
OUT:
	for i := 1; i <= max; i++ {
		for j := 2; j < i; j++ {
			if i%j == 0 {
				continue OUT
			}
		}
		total += i
	}
	return total
}

func getWords(n int) string {
	switch n {
	case 1:
		return "one"
	case 2:
		return "a couple"
	default:
		return "lots"
	}
}

func chain(a, b, c, d bool) int {
	if a && b && c || d {
		return 1
	} else if a {
		return 2
	} else {
		return 3
	}
}

func fact(n int) int {
	if n <= 1 {
		return 1
	}
	return n * fact(n-1)
}
`

func TestComplexity(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", sample, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]int{ // cyclomatic, cognitive
		"sumOfPrimes": {4, 7},
		"getWords":    {3, 1},
		"chain":       {6, 5},
		"fact":        {2, 2},
	}
	for _, d := range f.Decls {
		fd := d.(*ast.FuncDecl)
		w := want[fd.Name.Name]
		if got := cyclomatic(fd); got != w[0] {
			t.Errorf("%s: cyclomatic = %d, want %d", fd.Name.Name, got, w[0])
		}
		if got := cognitive(fd); got != w[1] {
			t.Errorf("%s: cognitive = %d, want %d", fd.Name.Name, got, w[1])
		}
	}
}

func TestLinesAndHalstead(t *testing.T) {
	li := scanLines([]byte("package p\n\n// doc\nfunc f() {}\n"))
	code, comment, blank := li.counts(1, li.total)
	if code != 2 || comment != 1 || blank != 1 {
		t.Fatalf("code %d comment %d blank %d", code, comment, blank)
	}
	h := halstead([]byte("func f(a int) int { return a + 1 }"))
	if h.Operands == 0 || h.Operators == 0 || h.Volume <= 0 {
		t.Fatalf("halstead = %+v", h)
	}
	if mi := maintainabilityIndex(h.Volume, 1, 1); mi <= 50 || mi > 100 {
		t.Fatalf("MI of a one-liner = %v", mi)
	}
}
