// Package mutate implements mutation testing for Go. It injects small
// faults (mutants) into source files and runs the package's tests against
// each one through go test -overlay, so the source tree is never modified.
// A mutant is killed when a test fails; a surviving mutant marks behaviour
// that no test checks, which statement coverage cannot reveal.
//
// The operators follow the classic set (conditional boundaries, negated
// comparisons, arithmetic, logical connectives, increments, removed
// negations and calls). Like Google's mutation testing, it skips "arid"
// code where a mutant is noise: logging calls and lock or lifecycle calls
// whose removal only produces hangs.
package mutate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Status is the outcome of one mutant.
type Status string

// Mutant outcomes.
const (
	Killed     Status = "killed"
	Survived   Status = "survived"
	TimedOut   Status = "timed_out"
	NotCovered Status = "not_covered"
	Invalid    Status = "invalid"
	Skipped    Status = "skipped"
	Equivalent Status = "equivalent"
)

// Mutant is one injected fault.
type Mutant struct {
	ID       int    `json:"id"`
	File     string `json:"file"` // slash path relative to the module root
	Line     int    `json:"line"`
	Col      int    `json:"col"`
	Operator string `json:"operator"`
	Original string `json:"original"`
	Mutated  string `json:"mutated"`
	Before   string `json:"before"`
	After    string `json:"after"`
	Status   Status `json:"status"`

	start, end int
	repl       string
}

// Detected reports whether the tests noticed the mutant.
func (m Mutant) Detected() bool { return m.Status == Killed || m.Status == TimedOut }

var binaryOps = map[token.Token]struct {
	repl string
	op   string
}{
	token.LSS:  {"<=", "boundary"},
	token.LEQ:  {"<", "boundary"},
	token.GTR:  {">=", "boundary"},
	token.GEQ:  {">", "boundary"},
	token.EQL:  {"!=", "negation"},
	token.NEQ:  {"==", "negation"},
	token.ADD:  {"-", "arithmetic"},
	token.SUB:  {"+", "arithmetic"},
	token.MUL:  {"/", "arithmetic"},
	token.QUO:  {"*", "arithmetic"},
	token.REM:  {"*", "arithmetic"},
	token.LAND: {"||", "logical"},
	token.LOR:  {"&&", "logical"},
}

// aridMethods are calls whose removal produces deadlocks, leaks or nothing
// observable rather than a meaningful fault.
var aridMethods = []string{"Lock", "Unlock", "RLock", "RUnlock", "Done", "Wait", "Add", "Close",
	"Stop", "Helper", "Cleanup", "Parallel", "Flush", "Sync"}

// Find returns the mutants of one Go source file. file is the path that
// will be recorded in the mutants.
func Find(file string, src []byte) ([]Mutant, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	tf := fset.File(f.Pos())
	lines := bytes.Split(src, []byte("\n"))
	var out []Mutant
	add := func(pos, end token.Pos, op, orig, repl, shown string) {
		p := fset.Position(pos)
		m := Mutant{File: file, Line: p.Line, Col: p.Column, Operator: op, Original: orig, Mutated: shown,
			start: tf.Offset(pos), end: tf.Offset(end), repl: repl}
		if p.Line-1 < len(lines) {
			m.Before = strings.TrimSpace(string(lines[p.Line-1]))
			after := Apply(src, m)
			if al := bytes.Split(after, []byte("\n")); p.Line-1 < len(al) {
				m.After = strings.TrimSpace(string(al[p.Line-1]))
			}
		}
		out = append(out, m)
	}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue // constants and package variables are not mutated
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if isLogging(x.Fun) {
					return false // arid: nothing inside a log call is behaviour
				}
			case *ast.BinaryExpr:
				r, ok := binaryOps[x.Op]
				if !ok || (r.op == "arithmetic" && (isString(x.X) || isString(x.Y) || (isConst(x.X) && isConst(x.Y)))) {
					break // concatenation, or constant arithmetic such as 5 * time.Minute
				}
				opText := x.Op.String()
				add(x.OpPos, x.OpPos+token.Pos(len(opText)), r.op, opText, r.repl, r.repl)
			case *ast.IncDecStmt:
				repl := "--"
				if x.Tok == token.DEC {
					repl = "++"
				}
				add(x.TokPos, x.TokPos+2, "incdec", x.Tok.String(), repl, repl)
			case *ast.UnaryExpr:
				if x.Op == token.NOT {
					add(x.OpPos, x.OpPos+1, "not-removal", "!", "", "(removed !)")
				}
			case *ast.Ident:
				switch x.Name {
				case "true":
					add(x.Pos(), x.End(), "bool-literal", "true", "false", "false")
				case "false":
					add(x.Pos(), x.End(), "bool-literal", "false", "true", "true")
				}
			case *ast.ExprStmt:
				if call, ok := x.X.(*ast.CallExpr); ok && !aridCall(call) {
					text := string(src[tf.Offset(x.Pos()):tf.Offset(x.End())])
					add(x.Pos(), x.End(), "call-removal", oneLine(text), "{}", "(call removed)")
				}
			}
			return true
		})
	}
	return out, nil
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}

// isString reports whether e is evidently a string, so + is concatenation.
func isString(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.BasicLit:
		return x.Kind == token.STRING
	case *ast.BinaryExpr:
		return x.Op == token.ADD && (isString(x.X) || isString(x.Y))
	case *ast.ParenExpr:
		return isString(x.X)
	case *ast.CallExpr:
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && (id.Name == "fmt" || id.Name == "strings" || id.Name == "strconv") {
				return strings.HasPrefix(sel.Sel.Name, "Sprint") || sel.Sel.Name == "Join" || sel.Sel.Name == "Repeat" ||
					sel.Sel.Name == "Itoa" || strings.HasPrefix(sel.Sel.Name, "Format") || strings.HasPrefix(sel.Sel.Name, "Trim")
			}
		}
	}
	return false
}

// isConst reports whether e is evidently a constant: a literal, a qualified
// constant such as time.Second, or arithmetic on those.
func isConst(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.BasicLit:
		return true
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		return ok && id.Name == "time"
	case *ast.BinaryExpr:
		return isConst(x.X) && isConst(x.Y)
	case *ast.ParenExpr:
		return isConst(x.X)
	}
	return false
}

func isLogging(fun ast.Expr) bool {
	for e := fun; ; {
		switch x := e.(type) {
		case *ast.SelectorExpr:
			if n := strings.ToLower(x.Sel.Name); n == "log" || n == "logger" || n == "slog" {
				return true
			}
			e = x.X
		case *ast.Ident:
			n := strings.ToLower(x.Name)
			return n == "log" || n == "slog" || n == "logger"
		case *ast.CallExpr:
			e = x.Fun
		default:
			return false
		}
	}
}

func aridCall(call *ast.CallExpr) bool {
	if isLogging(call.Fun) {
		return true
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok && slices.Contains(aridMethods, sel.Sel.Name) {
		return true
	}
	if id, ok := call.Fun.(*ast.Ident); ok && (id.Name == "panic" || id.Name == "close" || id.Name == "cancel") {
		return true
	}
	return false
}

// Apply returns src with the mutant applied.
func Apply(src []byte, m Mutant) []byte {
	out := make([]byte, 0, len(src)+len(m.repl))
	out = append(out, src[:m.start]...)
	out = append(out, m.repl...)
	return append(out, src[m.end:]...)
}

// Sample picks up to n mutants, spreading them over files and operators so
// that a budget still covers all of the code. It reports whether it dropped any.
func Sample(ms []Mutant, n int) ([]Mutant, bool) {
	if n <= 0 || len(ms) <= n {
		return ms, false
	}
	var keys []string
	groups := map[string][]Mutant{}
	for _, m := range ms {
		k := m.File + "|" + m.Operator
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], m)
	}
	var out []Mutant
	for round := 0; len(out) < n; round++ {
		progressed := false
		for _, k := range keys {
			if g := groups[k]; round < len(g) && len(out) < n {
				// Spread picks within a group too: take from evenly spaced positions.
				out = append(out, g[(round*7919)%len(g)])
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}
	slices.SortFunc(out, func(a, b Mutant) int {
		if c := strings.Compare(a.File, b.File); c != 0 {
			return c
		}
		return a.start - b.start
	})
	out = slices.CompactFunc(out, func(a, b Mutant) bool { return a.File == b.File && a.start == b.start && a.repl == b.repl })
	return out, true
}

// Coverage reads a Go coverage profile and returns a function reporting
// whether a line of a file (relative to the module root) ran in the tests.
func Coverage(profile, module string) (func(file string, line int) bool, error) {
	b, err := os.ReadFile(profile)
	if err != nil {
		return nil, err
	}
	type span struct{ from, to int }
	covered := map[string][]span{}
	for _, l := range strings.Split(string(b), "\n")[1:] {
		// path/file.go:12.3,15.4 2 1
		colon := strings.LastIndex(l, ":")
		fields := strings.Fields(l)
		if colon < 0 || len(fields) != 3 || fields[2] == "0" {
			continue
		}
		file := strings.TrimPrefix(l[:colon], module+"/")
		rng := strings.SplitN(strings.Fields(l[colon+1:])[0], ",", 2)
		if len(rng) != 2 {
			continue
		}
		from, _ := strconv.Atoi(strings.SplitN(rng[0], ".", 2)[0])
		to, _ := strconv.Atoi(strings.SplitN(rng[1], ".", 2)[0])
		covered[file] = append(covered[file], span{from, to})
	}
	return func(file string, line int) bool {
		for _, s := range covered[file] {
			if line >= s.from && line <= s.to {
				return true
			}
		}
		return false
	}, nil
}

// Runner executes mutants against the tests of their packages.
type Runner struct {
	Root     string        // module root
	Env      []string      // environment for go test
	Args     []string      // extra go test flags, e.g. -skip for pending tests
	Workers  int           // parallel go test processes
	Timeout  time.Duration // per mutant test run
	Budget   time.Duration // mutants not started within the budget are skipped
	Covered  func(file string, line int) bool
	Prepare  func(*exec.Cmd) // e.g. process-group isolation
	Progress func(done, total int, m Mutant)
	// Command, when set, builds the go command instead (args start after
	// "go"), e.g. to run it in a sandbox; Env and Prepare are then unused.
	Command func(ctx context.Context, dir string, args []string) *exec.Cmd
	TempDir string // where mutant sources and overlays are written; default os.TempDir
	// Cache, when set, answers mutants whose code and tests are unchanged
	// since an earlier run, so they are not compiled and tested again.
	Cache Cache
}

// Cache remembers mutant outcomes. Get reports a stored status; Put stores
// one. Implementations key on everything the outcome depends on.
type Cache interface {
	Get(m Mutant) (Status, bool)
	Put(m Mutant, s Status)
}

// cacheable reports whether a status is a stable outcome worth storing.
func cacheable(s Status) bool { return s == Killed || s == Survived || s == TimedOut || s == Invalid }

// command builds one go command.
func (r *Runner) command(ctx context.Context, args []string) *exec.Cmd {
	if r.Command != nil {
		return r.Command(ctx, r.Root, args)
	}
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir, cmd.Env = r.Root, r.Env
	if r.Prepare != nil {
		r.Prepare(cmd)
	}
	return cmd
}

// Baseline runs the tests of each package once without mutants. It returns
// an error if they fail, since mutants cannot be judged against failing tests.
func (r *Runner) Baseline(ctx context.Context, pkgs []string) (time.Duration, error) {
	start := time.Now()
	args := append([]string{"test", "-count=1", "-vet=off"}, r.Args...)
	if out, err := r.command(ctx, append(args, pkgs...)).CombinedOutput(); err != nil {
		return 0, fmt.Errorf("tests fail without mutants: %s", lastLines(string(out), 8))
	}
	return time.Since(start), nil
}

// Run executes the mutants and returns them with their status set.
func (r *Runner) Run(ctx context.Context, ms []Mutant) ([]Mutant, error) {
	tmp, err := os.MkdirTemp(r.TempDir, "fliessband-mutants-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	// The go command reports and matches overlay paths with symlinks resolved
	// (macOS temp directories live behind /var -> /private/var).
	if real, err := filepath.EvalSymlinks(r.Root); err == nil {
		r.Root = real
	}
	sources := map[string][]byte{}
	for _, m := range ms {
		if _, ok := sources[m.File]; !ok {
			b, err := os.ReadFile(filepath.Join(r.Root, filepath.FromSlash(m.File)))
			if err != nil {
				return nil, err
			}
			sources[m.File] = b
		}
	}
	workers := max(r.Workers, 1)
	deadline := time.Now().Add(r.Budget)
	jobs := make(chan int)
	var mu sync.Mutex
	done := 0
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range jobs {
				r.runOne(ctx, tmp, &ms[i], sources[ms[i].File])
				if r.Cache != nil && cacheable(ms[i].Status) {
					r.Cache.Put(ms[i], ms[i].Status)
				}
				mu.Lock()
				done++
				if r.Progress != nil {
					r.Progress(done, len(ms), ms[i])
				}
				mu.Unlock()
			}
		})
	}
	for i := range ms {
		switch {
		case r.Covered != nil && !r.Covered(ms[i].File, ms[i].Line):
			ms[i].Status = NotCovered
		case ctx.Err() != nil || (r.Budget > 0 && time.Now().After(deadline)):
			ms[i].Status = Skipped
		case r.Cache != nil && r.cached(&ms[i]):
			mu.Lock()
			done++
			if r.Progress != nil {
				r.Progress(done, len(ms), ms[i])
			}
			mu.Unlock()
			continue
		default:
			jobs <- i
			continue
		}
		mu.Lock()
		done++
		mu.Unlock()
	}
	close(jobs)
	wg.Wait()
	return ms, ctx.Err()
}

func (r *Runner) cached(m *Mutant) bool {
	st, ok := r.Cache.Get(*m)
	if ok {
		m.Status = st
	}
	return ok
}

func (r *Runner) runOne(ctx context.Context, tmp string, m *Mutant, src []byte) {
	file := filepath.Join(tmp, fmt.Sprintf("mutant%d.go", m.ID))
	overlay := filepath.Join(tmp, fmt.Sprintf("mutant%d.json", m.ID))
	abs, _ := filepath.Abs(filepath.Join(r.Root, filepath.FromSlash(m.File)))
	ov, _ := json.Marshal(map[string]map[string]string{"Replace": {abs: file}})
	if os.WriteFile(file, Apply(src, *m), 0o600) != nil || os.WriteFile(overlay, ov, 0o600) != nil {
		m.Status = Skipped
		return
	}
	defer func() { _, _ = os.Remove(file), os.Remove(overlay) }()
	runCtx, cancel := context.WithTimeout(ctx, r.Timeout+time.Minute) // headroom for compiling
	defer cancel()
	args := append([]string{"test", "-overlay=" + overlay, "-count=1", "-failfast", "-vet=off",
		"-timeout=" + r.Timeout.String()}, r.Args...)
	args = append(args, "./"+filepath.ToSlash(filepath.Dir(m.File)))
	out, err := r.command(runCtx, args).CombinedOutput()
	switch {
	case err == nil:
		m.Status = Survived
	case ctx.Err() != nil:
		m.Status = Skipped
	case errors.Is(runCtx.Err(), context.DeadlineExceeded) || bytes.Contains(out, []byte("test timed out")):
		m.Status = TimedOut
	case bytes.Contains(out, []byte("[build failed]")) || bytes.Contains(out, []byte("[setup failed]")):
		m.Status = Invalid
	default:
		m.Status = Killed
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
