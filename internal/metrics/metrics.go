// Package metrics measures a generated service: size, complexity,
// maintainability, tests, coverage, findings, runtime, and a quality score.
package metrics

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Report is everything measured about one run.
type Report struct {
	GeneratedAt  time.Time    `json:"generated_at"`
	Size         Size         `json:"size"`
	Complexity   Complexity   `json:"complexity"`
	Functions    []Func       `json:"functions"`
	Coverage     *Coverage    `json:"coverage,omitempty"`
	Tests        *Tests       `json:"tests,omitempty"`
	Findings     Findings     `json:"findings"`
	Build        Build        `json:"build"`
	Runtime      *Runtime     `json:"runtime,omitempty"`
	API          API          `json:"api"`
	Completeness Completeness `json:"completeness"`
	Acceptance   *Acceptance  `json:"acceptance,omitempty"`
	Mutation     *Mutation    `json:"mutation,omitempty"`
	Fuzz         *Fuzz        `json:"fuzz,omitempty"`
	Contract     *Contract    `json:"contract,omitempty"`
	Integration  *Integration `json:"integration,omitempty"`
	Supply       *Supply      `json:"supply,omitempty"`
	Security     *Security    `json:"security,omitempty"`
	Factory      Factory      `json:"factory"`
	Score        Score        `json:"score"`
}

// LineCount splits lines into code, comment and blank.
type LineCount struct {
	Files   int `json:"files"`
	Lines   int `json:"lines"`
	Code    int `json:"code"`
	Comment int `json:"comment"`
	Blank   int `json:"blank"`
}

func (l *LineCount) add(o LineCount) {
	l.Files += o.Files
	l.Lines += o.Lines
	l.Code += o.Code
	l.Comment += o.Comment
	l.Blank += o.Blank
}

// FileStat describes one file of the generated service.
type FileStat struct {
	Path   string `json:"path"`
	Origin string `json:"origin"` // template, buf, llm, tool
	Kind   string `json:"kind"`   // code, test, generated, proto, config
	Bytes  int    `json:"bytes"`
	LineCount
	Funcs int `json:"funcs"`
}

// Size summarises how much code the factory produced.
type Size struct {
	Files    []FileStat           `json:"files"`
	ByOrigin map[string]LineCount `json:"by_origin"`
	ByKind   map[string]LineCount `json:"by_kind"`
	Total    LineCount            `json:"total"`
}

// Func holds per-function measurements.
type Func struct {
	Name            string   `json:"name"`
	File            string   `json:"file"`
	Line            int      `json:"line"`
	EndLine         int      `json:"end_line"`
	LOC             int      `json:"loc"`
	Params          int      `json:"params"`
	Cyclomatic      int      `json:"cyclomatic"`
	Cognitive       int      `json:"cognitive"`
	Halstead        Halstead `json:"halstead"`
	Maintainability float64  `json:"mi"`
	Test            bool     `json:"test"`
	Origin          string   `json:"origin"`
	Coverage        *float64 `json:"coverage,omitempty"`
}

// Bucket is one histogram bar.
type Bucket struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// Complexity summarises production (non-test, non-generated) functions.
type Complexity struct {
	Funcs          int      `json:"funcs"`
	TestFuncs      int      `json:"test_funcs"`
	MeanCyclomatic float64  `json:"mean_cyclomatic"`
	P90Cyclomatic  float64  `json:"p90_cyclomatic"`
	MaxCyclomatic  int      `json:"max_cyclomatic"`
	MeanCognitive  float64  `json:"mean_cognitive"`
	MaxCognitive   int      `json:"max_cognitive"`
	OverCyclomatic int      `json:"over_cyclomatic"` // > 10
	OverCognitive  int      `json:"over_cognitive"`  // > 15
	MeanMI         float64  `json:"mean_mi"`
	MinMI          float64  `json:"min_mi"`
	MeanLOC        float64  `json:"mean_loc"`
	MaxLOC         int      `json:"max_loc"`
	HalsteadVolume float64  `json:"halstead_volume"`
	HalsteadEffort float64  `json:"halstead_effort"`
	CyclomaticHist []Bucket `json:"cyclomatic_hist"`
	CognitiveHist  []Bucket `json:"cognitive_hist"`
	MaintainHist   []Bucket `json:"mi_hist"`
}

// Coverage is statement coverage from the test run.
type Coverage struct {
	Percent    float64       `json:"percent"` // all non-generated code
	Statements int           `json:"statements"`
	Covered    int           `json:"covered"`
	Packages   []PkgCoverage `json:"packages"`
}

// PkgCoverage is coverage of one package.
type PkgCoverage struct {
	Package    string  `json:"package"`
	Statements int     `json:"statements"`
	Covered    int     `json:"covered"`
	Percent    float64 `json:"percent"`
}

// Tests summarises go test -json output.
type Tests struct {
	Total    int        `json:"total"`
	TopLevel int        `json:"top_level"`
	Passed   int        `json:"passed"`
	Failed   int        `json:"failed"`
	Skipped  int        `json:"skipped"`
	Slowest  []TestCase `json:"slowest"`
	Failures []TestCase `json:"failures"`
}

// TestCase is one test result.
type TestCase struct {
	Name    string  `json:"name"`
	Package string  `json:"package"`
	Seconds float64 `json:"seconds"`
	Output  string  `json:"output,omitempty"`
}

// Finding is one issue reported by a tool.
type Finding struct {
	Tool    string `json:"tool"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// Findings groups tool findings.
type Findings struct {
	BufLint     []Finding `json:"buf_lint"`
	Vet         []Finding `json:"vet"`
	Lint        []Finding `json:"lint"`                  // golangci-lint (or staticcheck)
	Staticcheck []Finding `json:"staticcheck,omitempty"` // runs recorded before golangci-lint
	Modernize   []Finding `json:"modernize"`             // go fix modernizers
	Vulns       []Finding `json:"vulns"`
	Policy      []Finding `json:"policy"`
	Fuzz        []Finding `json:"fuzz"`
}

// Code returns the findings about the code itself (not dependencies).
func (f Findings) Code() int {
	return len(f.BufLint) + len(f.Vet) + len(f.Lint) + len(f.Staticcheck) + len(f.Policy) + len(f.Fuzz)
}

// Count returns the number of findings of all tools.
func (f Findings) Count() int {
	return f.Code() + len(f.Modernize) + len(f.Vulns)
}

// Build captures build outputs.
type Build struct {
	BinaryBytes  int64  `json:"binary_bytes"`
	DirectDeps   int    `json:"direct_deps"`
	IndirectDeps int    `json:"indirect_deps"`
	GoVersion    string `json:"go_version"`
}

// Runtime captures the smoke test of the running binary.
type Runtime struct {
	Store           string   `json:"store"`
	FirstExecMs     float64  `json:"first_exec_ms"`
	StartupMs       float64  `json:"startup_ms"`
	HealthStatus    string   `json:"health_status"`
	Services        []string `json:"services"`
	LoadMethod      string   `json:"load_method"`
	LoadCalls       int      `json:"load_calls"`
	LoadConcurrency int      `json:"load_concurrency"`
	LoadErrors      int      `json:"load_errors"`
	RPS             float64  `json:"rps"`
	P50Ms           float64  `json:"p50_ms"`
	P90Ms           float64  `json:"p90_ms"`
	P99Ms           float64  `json:"p99_ms"`
	RSSBytes        int64    `json:"rss_bytes"`
	ShutdownMs      float64  `json:"shutdown_ms"`
	CleanExit       bool     `json:"clean_exit"`
}

// API counts the surface defined by the spec.
type API struct {
	Services int `json:"services"`
	RPCs     int `json:"rpcs"`
	CRUD     int `json:"crud"`
	Custom   int `json:"custom"`
	Messages int `json:"messages"`
	Fields   int `json:"fields"`
}

// Completeness tracks how many RPCs have real implementations.
type Completeness struct {
	Total       int      `json:"total"`
	Implemented int      `json:"implemented"`
	Stubbed     []string `json:"stubbed"`
}

// Factory measures the factory itself.
type Factory struct {
	StageMs        map[string]float64 `json:"stage_ms"`
	TotalMs        float64            `json:"total_ms"`
	LLMModel       string             `json:"llm_model,omitempty"`
	LLMCalls       int                `json:"llm_calls"`
	TokensIn       int                `json:"tokens_in"`
	TokensOut      int                `json:"tokens_out"`
	TokensCached   int                `json:"tokens_cached"`
	RepairAttempts int                `json:"repair_attempts"`
	FirstPass      bool               `json:"first_pass"`
	RejectedWrites []string           `json:"rejected_writes,omitempty"`
	Prompts        []PromptRecord     `json:"prompts,omitempty"`
	Hardening      *Hardening         `json:"hardening,omitempty"`
	CostUSD        float64            `json:"cost_usd,omitempty"` // as reported by the provider
	Tournament     *Tournament        `json:"tournament,omitempty"`
	Review         *Review            `json:"review,omitempty"`
	Sandbox        *Sandbox           `json:"sandbox,omitempty"`
}

// Sandbox records how the factory contained the generated code it executed
// (tests, fuzzing, mutants, the built server).
type Sandbox struct {
	Mode      string `json:"mode"` // none or docker
	Contained bool   `json:"contained"`
	Engine    string `json:"engine,omitempty"`
	Image     string `json:"image,omitempty"`
	Runtime   string `json:"runtime,omitempty"` // OCI runtime, e.g. runsc (gVisor)
	Network   string `json:"network,omitempty"` // network of the execution steps
	Detail    string `json:"detail,omitempty"`
}

// PassCount is how many of a set of tests passed.
type PassCount struct {
	Passed int `json:"passed"`
	Total  int `json:"total"`
}

func (p PassCount) String() string {
	if p.Total == 0 {
		return "–"
	}
	return fmt.Sprintf("%d/%d", p.Passed, p.Total)
}

// Tournament records how competing models' implementations were judged.
type Tournament struct {
	Winner     string      `json:"winner"`
	Candidates []Candidate `json:"candidates"`
}

// Candidate is one model's entry in the tournament.
type Candidate struct {
	Model       string               `json:"model"`
	Status      string               `json:"status"` // ran, compile, policy, incomplete, no answer
	Note        string               `json:"note,omitempty"`
	Own         PassCount            `json:"own"`                    // its own tests on its own code
	Visible     PassCount            `json:"visible"`                // visible spec examples
	Foreign     PassCount            `json:"foreign"`                // every other model's tests on its code
	Cross       map[string]PassCount `json:"cross,omitempty"`        // per test author
	FailedTests []string             `json:"failed_tests,omitempty"` // other models' tests it fails, as "author: TestName"
	Score       float64              `json:"score"`                  // selection score, 0..1; negative when it did not run
	TokensIn    int                  `json:"tokens_in"`
	TokensOut   int                  `json:"tokens_out"`
	CostUSD     float64              `json:"cost_usd,omitempty"`
	Ms          float64              `json:"ms"`
	Chosen      bool                 `json:"chosen"`
}

// Review is a second model's review of the LLM-written code.
type Review struct {
	Model    string          `json:"model"`
	Findings []ReviewFinding `json:"findings"`
	Note     string          `json:"note,omitempty"`
}

// ReviewFinding is one problem the reviewer reported.
type ReviewFinding struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Severity string `json:"severity"` // bug, risk, nit
	Message  string `json:"message"`
}

// PromptRecord identifies one LLM call for provenance: what was asked, by
// digest, and why.
type PromptRecord struct {
	Purpose string `json:"purpose"` // implement, repair, harden
	SHA256  string `json:"sha256"`
	Model   string `json:"model"`
}

// Hardening reports the mutation-guided test round: survivors in
// LLM-written code are sent back with a request for tests that kill them.
type Hardening struct {
	Survivors  int    `json:"survivors"`
	Killed     int    `json:"killed"`
	Equivalent int    `json:"equivalent"` // judged unkillable by the model
	Kept       bool   `json:"kept"`       // the new tests passed and were kept
	Note       string `json:"note,omitempty"`
}

var reGenerated = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.$`)

// AnalyzeSource measures every file below dir. origin maps a slash path to
// its origin (from the manifest); unknown paths count as "template".
func AnalyzeSource(dir string, origin func(string) string) (Size, []Func, error) {
	size := Size{ByOrigin: map[string]LineCount{}, ByKind: map[string]LineCount{}}
	var funcs []Func
	root, err := os.OpenRoot(dir) // generated code cannot lead the walk outside dir
	if err != nil {
		return size, nil, err
	}
	defer func() { _ = root.Close() }()
	fsys := root.FS()
	err = fs.WalkDir(fsys, ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel == ".fliessband" || rel == "bin" || strings.HasPrefix(d.Name(), ".") && rel != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if rel == "go.sum" || rel == "coverage.out" || !d.Type().IsRegular() {
			return nil
		}
		src, err := fs.ReadFile(fsys, rel)
		if err != nil {
			return err
		}
		fsStat := FileStat{Path: rel, Bytes: len(src), Origin: origin(rel)}
		if fsStat.Origin == "" {
			fsStat.Origin = "template"
		}
		switch {
		case strings.HasSuffix(rel, ".go"):
			li := scanLines(src)
			code, comment, blank := li.counts(1, li.total)
			fsStat.LineCount = LineCount{Files: 1, Lines: li.total, Code: code, Comment: comment, Blank: blank}
			switch {
			case reGenerated.Match(src):
				fsStat.Kind = "generated"
				if fsStat.Origin == "template" {
					fsStat.Origin = "buf"
				}
			case strings.HasSuffix(rel, "_test.go"):
				fsStat.Kind = "test"
			default:
				fsStat.Kind = "code"
			}
			if fsStat.Kind != "generated" {
				fns, err := analyzeFuncs(rel, src, li, fsStat.Kind == "test", fsStat.Origin)
				if err != nil {
					return fmt.Errorf("%s: %w", rel, err)
				}
				fsStat.Funcs = len(fns)
				funcs = append(funcs, fns...)
			}
		case strings.HasSuffix(rel, ".proto"):
			fsStat.Kind = "proto"
			fsStat.LineCount = countPlain(src, "//")
		default:
			fsStat.Kind = "config"
			fsStat.LineCount = countPlain(src, "#")
		}
		size.Files = append(size.Files, fsStat)
		bo := size.ByOrigin[fsStat.Origin]
		bo.add(fsStat.LineCount)
		size.ByOrigin[fsStat.Origin] = bo
		bk := size.ByKind[fsStat.Kind]
		bk.add(fsStat.LineCount)
		size.ByKind[fsStat.Kind] = bk
		size.Total.add(fsStat.LineCount)
		return nil
	})
	return size, funcs, err
}

func countPlain(src []byte, commentPrefix string) LineCount {
	lc := LineCount{Files: 1}
	sc := bufio.NewScanner(bytes.NewReader(src))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		lc.Lines++
		t := strings.TrimSpace(sc.Text())
		switch {
		case t == "":
			lc.Blank++
		case strings.HasPrefix(t, commentPrefix):
			lc.Comment++
		default:
			lc.Code++
		}
	}
	return lc
}

func analyzeFuncs(rel string, src []byte, li lineInfo, test bool, origin string) ([]Func, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var out []Func
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		start, end := fset.Position(fd.Pos()), fset.Position(fd.End())
		code, _, _ := li.counts(start.Line, end.Line)
		h := halstead(src[start.Offset:end.Offset])
		cc := cyclomatic(fd)
		fn := Func{
			Name:       funcName(fd),
			File:       rel,
			Line:       start.Line,
			EndLine:    end.Line,
			LOC:        code,
			Params:     fd.Type.Params.NumFields(),
			Cyclomatic: cc,
			Cognitive:  cognitive(fd),
			Halstead:   h,
			Test:       test,
			Origin:     origin,
		}
		fn.Maintainability = round(maintainabilityIndex(h.Volume, cc, code), 1)
		out = append(out, fn)
	}
	return out, nil
}

func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if ix, ok := t.(*ast.IndexExpr); ok {
		t = ix.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

// Summarize computes complexity aggregates over production functions.
func Summarize(funcs []Func) Complexity {
	var c Complexity
	var cc, cog, mi, loc []float64
	for _, f := range funcs {
		if f.Test {
			c.TestFuncs++
			continue
		}
		c.Funcs++
		cc = append(cc, float64(f.Cyclomatic))
		cog = append(cog, float64(f.Cognitive))
		mi = append(mi, f.Maintainability)
		loc = append(loc, float64(f.LOC))
		c.MaxCyclomatic = max(c.MaxCyclomatic, f.Cyclomatic)
		c.MaxCognitive = max(c.MaxCognitive, f.Cognitive)
		c.MaxLOC = max(c.MaxLOC, f.LOC)
		if f.Cyclomatic > 10 {
			c.OverCyclomatic++
		}
		if f.Cognitive > 15 {
			c.OverCognitive++
		}
		c.HalsteadVolume += f.Halstead.Volume
		c.HalsteadEffort += f.Halstead.Effort
	}
	c.MeanCyclomatic = round(mean(cc), 2)
	c.P90Cyclomatic = percentile(cc, 0.9)
	c.MeanCognitive = round(mean(cog), 2)
	c.MeanMI = round(mean(mi), 1)
	if len(mi) > 0 {
		c.MinMI = slices.Min(mi)
	}
	c.MeanLOC = round(mean(loc), 1)
	c.HalsteadVolume = round(c.HalsteadVolume, 0)
	c.HalsteadEffort = round(c.HalsteadEffort, 0)
	c.CyclomaticHist = histogram(cc, []float64{1, 2, 5, 8, 11, 16}, []string{"1", "2–4", "5–7", "8–10", "11–15", "16+"})
	c.CognitiveHist = histogram(cog, []float64{0, 1, 5, 10, 16, 25}, []string{"0", "1–4", "5–9", "10–15", "16–24", "25+"})
	c.MaintainHist = histogram(mi, []float64{0, 20, 40, 60, 80}, []string{"0–19", "20–39", "40–59", "60–79", "80–100"})
	return c
}

func histogram(vals, lows []float64, labels []string) []Bucket {
	out := make([]Bucket, len(lows))
	for i := range lows {
		out[i].Label = labels[i]
	}
	for _, v := range vals {
		for i := len(lows) - 1; i >= 0; i-- {
			if v >= lows[i] {
				out[i].Count++
				break
			}
		}
	}
	return out
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var s float64
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := slices.Clone(v)
	sort.Float64s(s)
	idx := int(math.Ceil(p*float64(len(s)))) - 1
	return s[max(0, min(idx, len(s)-1))]
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// ParseCoverProfile reads a Go cover profile, skipping generated files and
// files outside the module, and sets per-function coverage on funcs.
func ParseCoverProfile(path, module string, generated func(string) bool, funcs []Func) (*Coverage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	type block struct {
		file               string
		startLine, endLine int
		stmts              int
		hit                bool
	}
	blocks := map[string]*block{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "mode:") || line == "" {
			continue
		}
		// file.go:12.34,15.2 3 1
		colon := strings.LastIndex(line, ":")
		fields := strings.Fields(line[colon+1:])
		if colon < 0 || len(fields) != 3 {
			continue
		}
		file := strings.TrimPrefix(line[:colon], module+"/")
		if generated(file) {
			continue
		}
		var sl, sc2, el, ec int
		if _, err := fmt.Sscanf(fields[0], "%d.%d,%d.%d", &sl, &sc2, &el, &ec); err != nil {
			continue
		}
		stmts, _ := strconv.Atoi(fields[1])
		count, _ := strconv.Atoi(fields[2])
		key := line[:colon] + ":" + fields[0]
		b, ok := blocks[key]
		if !ok {
			b = &block{file: file, startLine: sl, endLine: el, stmts: stmts}
			blocks[key] = b
		}
		b.hit = b.hit || count > 0
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	cov := &Coverage{}
	pkgs := map[string]*PkgCoverage{}
	type agg struct{ stmts, hit int }
	perFunc := make([]agg, len(funcs))
	for _, b := range blocks {
		pkg := filepath.ToSlash(filepath.Dir(b.file))
		pc, ok := pkgs[pkg]
		if !ok {
			pc = &PkgCoverage{Package: pkg}
			pkgs[pkg] = pc
		}
		pc.Statements += b.stmts
		cov.Statements += b.stmts
		if b.hit {
			pc.Covered += b.stmts
			cov.Covered += b.stmts
		}
		for i, fn := range funcs {
			if fn.File == b.file && b.startLine >= fn.Line && b.endLine <= fn.EndLine {
				perFunc[i].stmts += b.stmts
				if b.hit {
					perFunc[i].hit += b.stmts
				}
				break
			}
		}
	}
	for i := range funcs {
		if perFunc[i].stmts > 0 {
			p := round(100*float64(perFunc[i].hit)/float64(perFunc[i].stmts), 1)
			funcs[i].Coverage = &p
		}
	}
	for _, pc := range pkgs {
		if pc.Statements > 0 {
			pc.Percent = round(100*float64(pc.Covered)/float64(pc.Statements), 1)
		}
		cov.Packages = append(cov.Packages, *pc)
	}
	slices.SortFunc(cov.Packages, func(a, b PkgCoverage) int { return strings.Compare(a.Package, b.Package) })
	if cov.Statements > 0 {
		cov.Percent = round(100*float64(cov.Covered)/float64(cov.Statements), 1)
	}
	return cov, nil
}

// ServicePercent is coverage of internal/ packages only (the hand-off code).
func (c *Coverage) ServicePercent() float64 {
	var s, h int
	for _, p := range c.Packages {
		if strings.HasPrefix(p.Package, "internal/") {
			s += p.Statements
			h += p.Covered
		}
	}
	if s == 0 {
		return 0
	}
	return round(100*float64(h)/float64(s), 1)
}

type testEvent struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Elapsed float64 `json:"Elapsed"`
	Output  string  `json:"Output"`
}

// ParseTestJSON summarises go test -json output. If keep is given, only
// tests whose name it accepts are counted.
func ParseTestJSON(r io.Reader, module string, keep ...func(test string) bool) *Tests {
	t := &Tests{}
	output := map[string]*strings.Builder{}
	var all []TestCase
	dec := json.NewDecoder(r)
	for {
		var ev testEvent
		if err := dec.Decode(&ev); err != nil {
			break
		}
		if ev.Test == "" || (len(keep) > 0 && !keep[0](ev.Test)) {
			continue
		}
		key := ev.Package + " " + ev.Test
		pkg := strings.TrimPrefix(strings.TrimPrefix(ev.Package, module), "/")
		switch ev.Action {
		case "output":
			b, ok := output[key]
			if !ok {
				b = &strings.Builder{}
				output[key] = b
			}
			if b.Len() < 4000 {
				b.WriteString(ev.Output)
			}
		case "pass", "fail", "skip":
			t.Total++
			if !strings.Contains(ev.Test, "/") {
				t.TopLevel++
			}
			tc := TestCase{Name: ev.Test, Package: pkg, Seconds: ev.Elapsed}
			switch ev.Action {
			case "pass":
				t.Passed++
			case "fail":
				t.Failed++
				if b := output[key]; b != nil {
					tc.Output = b.String()
				}
				t.Failures = append(t.Failures, tc)
			case "skip":
				t.Skipped++
			}
			all = append(all, tc)
		}
	}
	slices.SortFunc(all, func(a, b TestCase) int {
		if a.Seconds != b.Seconds {
			if a.Seconds > b.Seconds {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	t.Slowest = all[:min(5, len(all))]
	return t
}

// ParseGoMod counts direct and indirect requirements.
func ParseGoMod(path string) (direct, indirect int, goVersion string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, ""
	}
	inBlock := false
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "go "):
			goVersion = strings.TrimPrefix(line, "go ")
		case line == "require (":
			inBlock = true
		case inBlock && line == ")":
			inBlock = false
		case inBlock && line != "", strings.HasPrefix(line, "require "):
			if strings.Contains(line, "// indirect") {
				indirect++
			} else {
				direct++
			}
		}
	}
	return direct, indirect, goVersion
}

// Security is the audit of the generated service: each check reads the files
// the line produced, or a station's result, and cites where it looked.
type Security struct {
	Checks []SecurityCheck `json:"checks"`
	Passed int             `json:"passed"`
	Warned int             `json:"warned"`
	Failed int             `json:"failed"`
}

// SecurityCheck is one control: pass, warn, fail, or skip when it does not apply.
type SecurityCheck struct {
	ID     string `json:"id"`
	Area   string `json:"area"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
}
