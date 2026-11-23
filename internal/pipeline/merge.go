package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/tillknuesting/fliessband/internal/metrics"
	"github.com/tillknuesting/fliessband/internal/policy"
)

// After the tournament, tests that losing models wrote are not thrown away:
// those that passed when the cross-testing ran them against the winner's
// code are appended to the winner's test file, with the helpers they need
// from their own file. A test whose name is taken is renamed (an identical
// copy is dropped). The merge is kept only if the whole merged file then
// compiles and passes against the winner's code; otherwise the winner's
// own test file goes down the line unchanged.

// mergeTests merges losing candidates' passing tests into the winner's test
// file in win.files and records the outcome in t. It returns an error only
// when the factory itself fails, such as on cancellation.
func (x *executor) mergeTests(ctx context.Context, st *StageState, dir string, win *candidate, cands []*candidate, t *metrics.Tournament) error {
	if win.m.Status != "ran" || len(win.crossRuns) == 0 {
		return nil
	}
	m, merged, note := x.mergedTestFile(st, win, cands)
	if note != "" {
		t.MergeNote = note
		x.log(st, "merge: "+note)
	}
	if merged == "" {
		return nil
	}
	if vs := policy.Check(policy.Config{Module: x.spec.Module, PBPackage: x.model.GoPkgPath}, map[string][]byte{testFile: []byte(merged)}); len(vs) > 0 {
		t.MergeNote = "the merged test file breaks the policy (" + vs[0].String() + "); kept the winner's tests"
		x.log(st, "merge: "+t.MergeNote)
		return nil
	}
	results, out, err := x.candidateTest(ctx, st, dir, "merge", win.files[implFile], merged, "")
	if err != nil {
		return err
	}
	if results == nil {
		t.MergeNote = "the merged test file does not compile (" + compileError(out) + "); kept the winner's tests"
		x.log(st, "merge: "+t.MergeNote)
		return nil
	}
	for _, name := range testNames(merged) {
		if !results[name] {
			t.MergeNote = name + " fails in the merged test file; kept the winner's tests"
			x.log(st, "merge: "+t.MergeNote)
			return nil
		}
	}
	win.files[testFile] = merged
	t.MergedTests, t.MergedFrom, t.Merged = len(m.merged), m.from, m.merged
	x.log(st, fmt.Sprintf("merged %s from losing candidates into %s's tests: %s", fmtCount(len(m.merged), "passing test", "passing tests"), win.model, strings.Join(m.merged, "; ")))
	return nil
}

// mergedTestFile builds the winner's test file with the losers' passing
// tests appended. It returns "" when there is nothing to merge, with a note
// when that is because something went wrong.
func (x *executor) mergedTestFile(st *StageState, win *candidate, cands []*candidate) (*testMerger, string, string) {
	m, err := newTestMerger(win.files[implFile], win.files[testFile])
	if err != nil {
		return nil, "", "the winner's files do not parse: " + err.Error()
	}
	for _, c := range cands {
		results := win.crossRuns[c.model]
		if c == win || results == nil {
			continue
		}
		var passing []string
		for _, name := range c.tests {
			if results[name] {
				passing = append(passing, name)
			}
		}
		for _, skipped := range m.add(c.model, c.files[testFile], passing) {
			x.log(st, "merge: "+skipped)
		}
	}
	if len(m.merged) == 0 {
		return m, "", ""
	}
	merged, err := m.source()
	if err != nil {
		return m, "", "the merged test file does not format: " + err.Error()
	}
	return m, merged, ""
}

// testMerger appends declarations from other test files to one test file.
type testMerger struct {
	src     string
	fset    *token.FileSet
	file    *ast.File
	taken   map[string]string // package-level name in the winner's files or merged -> its source ("" when unknown)
	bodies  map[string]bool   // tests already in the file, without their names and doc comments
	imports map[string]string // local name -> import path in the merged file
	added   map[string]string // imports to add: local name -> path
	alias   map[string]bool   // added imports that need an explicit name
	decls   []string
	merged  []string
	from    map[string]int
}

func newTestMerger(impl, tests string) (*testMerger, error) {
	m := &testMerger{src: tests, fset: token.NewFileSet(), taken: map[string]string{}, bodies: map[string]bool{}, imports: map[string]string{},
		added: map[string]string{}, alias: map[string]bool{}, from: map[string]int{}}
	f, err := parser.ParseFile(m.fset, testFile, tests, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	m.file = f
	for name, p := range fileImports(f) {
		m.imports[name] = p
	}
	for name, d := range topLevel(f) {
		m.taken[name] = printDecl(m.fset, f, d)
		if fn, ok := d.(*ast.FuncDecl); ok && isTestFunc(fn) {
			m.bodies[testBody(m.fset, f, fn)] = true
		}
	}
	implFset := token.NewFileSet()
	fi, err := parser.ParseFile(implFset, implFile, impl, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	for name := range topLevel(fi) {
		m.taken[name] = ""
	}
	return m, nil
}

// add merges the named tests of one candidate's test file, with the
// helpers and imports they use. It returns why tests were left out.
func (m *testMerger) add(model, src string, tests []string) (skipped []string) {
	if len(tests) == 0 {
		return nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, testFile, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return []string{model + "'s tests do not parse"}
	}
	top := topLevel(f)
	methods := map[string][]*ast.FuncDecl{}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv != nil && len(fn.Recv.List) > 0 {
			if name := recvName(fn.Recv.List[0].Type); name != "" {
				methods[name] = append(methods[name], fn)
			}
		}
	}
	imports := fileImports(f)
	for _, name := range tests {
		fn, ok := top[name].(*ast.FuncDecl)
		if !ok {
			continue
		}
		// The helpers it needs from its own file, transitively.
		var helpers []ast.Decl
		seen := map[ast.Decl]bool{fn: true}
		used := map[string]bool{} // selector prefixes, to find imports
		queue := []ast.Node{fn}
		for len(queue) > 0 {
			n := queue[0]
			queue = queue[1:]
			ast.Inspect(n, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.SelectorExpr:
					if id, ok := n.X.(*ast.Ident); ok {
						used[id.Name] = true
					}
				case *ast.Ident:
					d, ok := top[n.Name]
					if !ok || seen[d] || isTestFunc(d) {
						return true
					}
					seen[d] = true
					helpers = append(helpers, d)
					queue = append(queue, d)
					for _, meth := range methods[n.Name] {
						if !seen[meth] {
							seen[meth] = true
							helpers = append(helpers, meth)
							queue = append(queue, meth)
						}
					}
				}
				return true
			})
		}
		// Imports: reuse the winner's, add missing ones, give up on clashes.
		need := map[string]string{}
		clash := ""
		for local := range used {
			p, ok := imports[local]
			if !ok {
				continue
			}
			if have, ok := m.imports[local]; ok {
				if have != p {
					clash = fmt.Sprintf("%s means %s here but %s in the winner's file", local, p, have)
				}
				continue
			}
			for l, have := range m.imports {
				if have == p {
					clash = fmt.Sprintf("%s is imported as %s in the winner's file", p, l)
				}
			}
			need[local] = p
		}
		if clash != "" {
			skipped = append(skipped, fmt.Sprintf("%s's %s left out: %s", model, name, clash))
			continue
		}
		// Helpers: an identical one already present is reused; a different
		// one with a taken name makes the test unmergeable.
		var newHelpers []string
		helperNames := map[string]string{}
		conflict := ""
		for _, d := range helpers {
			text := printDecl(fset, f, d)
			names := declNames(d)
			reuse := true
			for _, n := range names {
				have, ok := m.taken[n]
				if !ok {
					reuse = false
					continue
				}
				if have != text {
					conflict = n
				}
			}
			if conflict != "" {
				break
			}
			if !reuse {
				newHelpers = append(newHelpers, text)
				for _, n := range names {
					helperNames[n] = text
				}
			}
		}
		if conflict != "" {
			skipped = append(skipped, fmt.Sprintf("%s's %s left out: its helper %s clashes with the winner's code", model, name, conflict))
			continue
		}
		body := testBody(fset, f, fn)
		if m.bodies[body] {
			continue // the same test is already there, perhaps under another name
		}
		as := name
		text := printDecl(fset, f, fn)
		if hasKey(m.taken, name) {
			base := name + "From" + camel(model)
			as = base
			for i := 2; hasKey(m.taken, as); i++ {
				as = base + strconv.Itoa(i)
			}
			fn.Name.Name = as
			text = printDecl(fset, f, fn)
			fn.Name.Name = name
		}
		m.decls = append(m.decls, newHelpers...)
		m.decls = append(m.decls, text)
		maps.Copy(m.taken, helperNames)
		m.taken[as] = text
		m.bodies[body] = true
		for local, p := range need {
			m.imports[local] = p
			m.added[local] = p
			m.alias[local] = local != defaultImportName(p)
		}
		entry := model + ": " + name
		if as != name {
			entry += " as " + as
		}
		m.merged = append(m.merged, entry)
		m.from[model]++
	}
	return skipped
}

// source returns the merged, gofmt-ed test file.
func (m *testMerger) source() (string, error) {
	var b bytes.Buffer
	src := m.src
	if len(m.added) > 0 {
		var lines strings.Builder
		for _, local := range slices.Sorted(maps.Keys(m.added)) {
			if m.alias[local] {
				lines.WriteString("\t" + local + " ")
			} else {
				lines.WriteString("\t")
			}
			lines.WriteString(strconv.Quote(m.added[local]) + "\n")
		}
		var imp *ast.GenDecl
		for _, d := range m.file.Decls {
			if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.IMPORT {
				imp = g
				break
			}
		}
		switch {
		case imp != nil && imp.Lparen.IsValid():
			at := m.fset.Position(imp.Rparen).Offset
			src = src[:at] + lines.String() + src[at:]
		case imp != nil:
			at := m.fset.Position(imp.End()).Offset
			src = src[:at] + "\nimport (\n" + lines.String() + ")\n" + src[at:]
		default:
			at := m.fset.Position(m.file.Name.End()).Offset
			src = src[:at] + "\n\nimport (\n" + lines.String() + ")\n" + src[at:]
		}
	}
	b.WriteString(strings.TrimRight(src, "\n"))
	b.WriteString("\n")
	for _, d := range m.decls {
		b.WriteString("\n" + d + "\n")
	}
	out, err := format.Source(b.Bytes())
	return string(out), err
}

// topLevel maps the package-level names a file declares to their declarations.
func topLevel(f *ast.File) map[string]ast.Decl {
	out := map[string]ast.Decl{}
	for _, d := range f.Decls {
		for _, n := range declNames(d) {
			out[n] = d
		}
	}
	return out
}

func declNames(d ast.Decl) []string {
	var names []string
	switch d := d.(type) {
	case *ast.FuncDecl:
		if d.Recv == nil {
			names = append(names, d.Name.Name)
		}
	case *ast.GenDecl:
		for _, s := range d.Specs {
			switch s := s.(type) {
			case *ast.ValueSpec:
				for _, n := range s.Names {
					if n.Name != "_" {
						names = append(names, n.Name)
					}
				}
			case *ast.TypeSpec:
				names = append(names, s.Name.Name)
			}
		}
	}
	return names
}

func isTestFunc(d ast.Decl) bool {
	fn, ok := d.(*ast.FuncDecl)
	return ok && fn.Recv == nil && (strings.HasPrefix(fn.Name.Name, "Test") || strings.HasPrefix(fn.Name.Name, "Fuzz") || strings.HasPrefix(fn.Name.Name, "Benchmark"))
}

func recvName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvName(t.X)
	case *ast.IndexExpr:
		return recvName(t.X)
	case *ast.IndexListExpr:
		return recvName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return ""
}

// fileImports maps the local names of a file's imports to their paths.
func fileImports(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, s := range f.Imports {
		p, err := strconv.Unquote(s.Path.Value)
		if err != nil {
			continue
		}
		local := defaultImportName(p)
		if s.Name != nil {
			local = s.Name.Name
		}
		if local == "_" || local == "." {
			continue
		}
		out[local] = p
	}
	return out
}

// defaultImportName guesses a package's name from its path: the last
// element, or the one before a major version suffix.
func defaultImportName(p string) string {
	base := path.Base(p)
	if len(base) > 1 && base[0] == 'v' && strings.Trim(base[1:], "0123456789") == "" {
		base = path.Base(path.Dir(p))
	}
	return strings.ReplaceAll(base, "-", "_")
}

func printDecl(fset *token.FileSet, f *ast.File, d ast.Decl) string {
	var b bytes.Buffer
	if err := printer.Fprint(&b, fset, &printer.CommentedNode{Node: d, Comments: f.Comments}); err != nil {
		return ""
	}
	return b.String()
}

// testBody prints a test function without its name and doc comment, so
// copies of one test under different names compare equal.
func testBody(fset *token.FileSet, f *ast.File, fn *ast.FuncDecl) string {
	name, doc := fn.Name.Name, fn.Doc
	fn.Name.Name, fn.Doc = "_", nil
	defer func() { fn.Name.Name, fn.Doc = name, doc }()
	var b bytes.Buffer
	if err := printer.Fprint(&b, fset, fn); err != nil {
		return name
	}
	return b.String()
}

// camel turns a model name such as "vendor/model-1.5" into "VendorModel15".
func camel(s string) string {
	var b strings.Builder
	upper := true
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if upper {
				r = unicode.ToUpper(r)
			}
			b.WriteRune(r)
			upper = false
		default:
			upper = true
		}
	}
	return b.String()
}

func hasKey(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}
