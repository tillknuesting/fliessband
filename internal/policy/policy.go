// Package policy checks code written by the LLM before anything compiles or
// runs it. The rules keep generated code inside the service layer's job:
// no new dependencies, no programs, files or network, no build tricks, and
// tests that assert something and are never skipped. Messages say how to
// comply, because they go back to the model in the repair loop.
//
// Research on coding agents (for example ImpossibleBench, 2025) shows that
// models under test pressure sometimes special-case or weaken tests. The
// factory therefore never lets the model edit the tests it is judged by,
// and these rules catch the weak-test patterns in the tests it does write.
package policy

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
)

// Violation is one broken rule.
type Violation struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Rule    string `json:"rule"`
	Message string `json:"message"`
}

func (v Violation) String() string {
	return fmt.Sprintf("%s:%d: %s [%s]", v.File, v.Line, v.Message, v.Rule)
}

// stdAllowed are the standard library packages service code may use.
var stdAllowed = []string{
	"bytes", "cmp", "context", "encoding/base64", "encoding/json", "errors", "fmt", "iter",
	"log/slog", "maps", "math", "regexp", "slices", "sort", "strconv", "strings", "sync",
	"sync/atomic", "time", "unicode", "unicode/utf8", "uuid",
}

// testAllowed are the extra packages tests may use.
var testAllowed = []string{"testing", "testing/synctest"}

// modAllowed are the third-party packages service code may use. Prefixes
// end in a slash.
var modAllowed = []string{
	"google.golang.org/grpc/codes",
	"google.golang.org/grpc/status",
	"google.golang.org/genproto/googleapis/rpc/errdetails",
	"google.golang.org/protobuf/proto",
	"google.golang.org/protobuf/types/known/",
}

// Config describes the service whose files are checked.
type Config struct {
	Module    string // Go module path of the service
	PBPackage string // import path of the generated API package
}

func (c Config) allowed(path string, test bool) bool {
	switch {
	case slices.Contains(stdAllowed, path), test && slices.Contains(testAllowed, path):
		return true
	case path == c.PBPackage, path == c.Module+"/internal/store", path == c.Module+"/internal/auth":
		return true
	}
	for _, a := range modAllowed {
		if path == a || (strings.HasSuffix(a, "/") && strings.HasPrefix(path, a)) {
			return true
		}
	}
	return false
}

func (c Config) allowedList(test bool) string {
	list := slices.Clone(stdAllowed)
	if test {
		list = append(list, testAllowed...)
	}
	list = append(list, modAllowed...)
	list = append(list, c.PBPackage, c.Module+"/internal/store", c.Module+"/internal/auth (services with auth)")
	return strings.Join(list, ", ")
}

// Check returns the violations in the given files (path -> source).
func Check(cfg Config, files map[string][]byte) []Violation {
	var out []Violation
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		out = append(out, checkFile(cfg, p, files[p])...)
	}
	return out
}

func checkFile(cfg Config, path string, src []byte) []Violation {
	test := strings.HasSuffix(path, "_test.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return []Violation{{File: path, Line: 1, Rule: "syntax", Message: "the file does not parse: " + err.Error()}}
	}
	var out []Violation
	add := func(pos token.Pos, rule, format string, a ...any) {
		out = append(out, Violation{File: path, Line: fset.Position(pos).Line, Rule: rule, Message: fmt.Sprintf(format, a...)})
	}

	if f.Name.Name != "service" {
		add(f.Name.Pos(), "package", "the file must be in package service, not %s", f.Name.Name)
	}
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, "//go:") || strings.HasPrefix(c.Text, "// +build") || strings.HasPrefix(c.Text, "//line ") {
				add(c.Pos(), "directive", "remove the %q directive: compiler directives, build tags and line directives are not allowed in generated service code", strings.Fields(c.Text)[0])
			}
		}
	}
	for _, is := range f.Imports {
		p, _ := strconv.Unquote(is.Path.Value)
		switch {
		case p == "C":
			add(is.Pos(), "cgo", "remove import \"C\": cgo is not allowed")
		case is.Name != nil && is.Name.Name == ".":
			add(is.Pos(), "dot-import", "import %q without a dot: dot imports hide where names come from", p)
		case !cfg.allowed(p, test):
			add(is.Pos(), "import", "import %q is not allowed: service code may not add dependencies or reach the file system, network or other programs. Allowed: %s",
				p, cfg.allowedList(test))
		}
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fd.Recv == nil && fd.Name.Name == "init" {
			add(fd.Pos(), "init", "remove func init: package initialisation would run before every test and server start")
		}
		if fd.Body == nil {
			continue
		}
		if !test {
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "panic" {
						add(call.Pos(), "panic", "return a gRPC status error instead of calling panic")
					}
				}
				return true
			})
			continue
		}
		if !strings.HasPrefix(fd.Name.Name, "Test") || fd.Name.Name == "TestMain" {
			continue
		}
		asserts := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				name := fun.Sel.Name
				if strings.HasPrefix(name, "Skip") {
					add(call.Pos(), "skip", "remove t.%s: tests must run; a test that cannot pass is a failure to fix", name)
				}
				if strings.HasPrefix(name, "Error") || strings.HasPrefix(name, "Fatal") || strings.HasPrefix(name, "Fail") {
					asserts = true
				}
			case *ast.Ident:
				for _, p := range []string{"want", "assert", "check", "expect", "require", "must"} {
					if strings.HasPrefix(strings.ToLower(fun.Name), p) {
						asserts = true
					}
				}
			}
			return true
		})
		if !asserts {
			add(fd.Pos(), "assert", "%s makes no assertions: call t.Error/t.Fatal or a helper such as wantCode when the result is wrong", fd.Name.Name)
		}
	}
	return out
}
