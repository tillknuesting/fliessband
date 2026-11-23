// Package gen renders a service spec into a Go gRPC microservice source tree.
package gen

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"time"

	"github.com/tillknuesting/fliessband/internal/spec"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

// Origins of generated files.
const (
	OriginTemplate = "template"
	OriginBuf      = "buf"
	OriginLLM      = "llm"
)

// File is one generated file.
type File struct {
	Path     string
	Template string
	Data     []byte
}

type output struct {
	tmpl      string
	path      string // itself a template
	when      func(*Model) bool
	perEntity bool
}

var (
	hasEntities = func(m *Model) bool { return len(m.Entities) > 0 }
	hasPostgres = func(m *Model) bool { return m.Postgres }
	hasMethods  = func(m *Model) bool { return len(m.Methods) > 0 }
)

var outputs = []output{
	{tmpl: "go.mod.tmpl", path: "go.mod"},
	{tmpl: "buf.yaml.tmpl", path: "buf.yaml"},
	{tmpl: "buf.gen.yaml.tmpl", path: "buf.gen.yaml"},
	{tmpl: "service.proto.tmpl", path: "proto/{{.ProtoDir}}/{{.ProtoFile}}"},
	{tmpl: "main.go.tmpl", path: "cmd/server/main.go"},
	{tmpl: "config.go.tmpl", path: "internal/config/config.go"},
	{tmpl: "config_test.go.tmpl", path: "internal/config/config_test.go"},
	{tmpl: "server.go.tmpl", path: "internal/server/server.go"},
	{tmpl: "interceptors.go.tmpl", path: "internal/server/interceptors.go"},
	{tmpl: "tls.go.tmpl", path: "internal/server/tls.go"},
	{tmpl: "server_test.go.tmpl", path: "internal/server/server_test.go"},
	{tmpl: "store.go.tmpl", path: "internal/store/store.go"},
	{tmpl: "memory.go.tmpl", path: "internal/store/memory.go"},
	{tmpl: "store_test.go.tmpl", path: "internal/store/store_test.go", when: hasEntities},
	{tmpl: "memory_test.go.tmpl", path: "internal/store/memory_test.go", when: hasEntities},
	{tmpl: "postgres.go.tmpl", path: "internal/store/postgres.go", when: hasPostgres},
	{tmpl: "postgres_test.go.tmpl", path: "internal/store/postgres_test.go", when: hasPostgres},
	{tmpl: "migration.sql.tmpl", path: "internal/store/migrations/0001_init.sql", when: hasPostgres},
	{tmpl: "service.go.tmpl", path: "internal/service/service.go"},
	{tmpl: "service_test.go.tmpl", path: "internal/service/service_test.go"},
	{tmpl: "entity.go.tmpl", path: "internal/service/{{.E.File}}.go", perEntity: true},
	{tmpl: "entity_test.go.tmpl", path: "internal/service/{{.E.File}}_test.go", perEntity: true},
	{tmpl: "custom.go.tmpl", path: "internal/service/custom.go", when: hasMethods},
	{tmpl: "custom_test.go.tmpl", path: "internal/service/custom_test.go", when: hasMethods},
	{tmpl: "acceptance_test.go.tmpl", path: "internal/service/acceptance_test.go", when: func(m *Model) bool { return len(m.Examples) > 0 }},
	{tmpl: "fuzz_test.go.tmpl", path: "internal/service/fuzz_test.go", when: func(m *Model) bool { return m.AnyList() || m.AnyValidate() }},
	{tmpl: "telemetry.go.tmpl", path: "internal/telemetry/telemetry.go", when: func(m *Model) bool { return m.OTel }},
	{tmpl: "Dockerfile.tmpl", path: "Dockerfile", when: func(m *Model) bool { return m.Docker }},
	{tmpl: "dockerignore.tmpl", path: ".dockerignore", when: func(m *Model) bool { return m.Docker }},
	{tmpl: "k8s_deployment.yaml.tmpl", path: "deploy/k8s/deployment.yaml", when: func(m *Model) bool { return m.Kubernetes }},
	{tmpl: "k8s_service.yaml.tmpl", path: "deploy/k8s/service.yaml", when: func(m *Model) bool { return m.Kubernetes }},
	{tmpl: "k8s_pdb.yaml.tmpl", path: "deploy/k8s/pdb.yaml", when: func(m *Model) bool { return m.Kubernetes }},
	{tmpl: "k8s_kustomization.yaml.tmpl", path: "deploy/k8s/kustomization.yaml", when: func(m *Model) bool { return m.Kubernetes }},
	{tmpl: "ci.yml.tmpl", path: ".github/workflows/ci.yml", when: func(m *Model) bool { return m.CI }},
	{tmpl: "golangci.yml.tmpl", path: ".golangci.yml"},
	{tmpl: "Makefile.tmpl", path: "Makefile"},
	{tmpl: "README.md.tmpl", path: "README.md"},
	{tmpl: "gitignore.tmpl", path: ".gitignore"},
}

// TemplatesDigest is the SHA-256 of all templates, recorded in provenance so
// a run names the exact generator that produced it.
func TemplatesDigest() string {
	entries, _ := templateFS.ReadDir("templates")
	h := sha256.New()
	for _, e := range entries {
		b, _ := templateFS.ReadFile("templates/" + e.Name())
		_, _ = fmt.Fprintf(h, "%s %d\n", e.Name(), len(b))
		_, _ = h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// LLMOwnedFiles are the only files the implement stage may rewrite.
var LLMOwnedFiles = []string{"internal/service/custom.go", "internal/service/custom_test.go"}

var funcs = template.FuncMap{
	"comment": comment,
	"oneline": oneline,
}

type data struct {
	*Model
	E *EntityModel
}

// File is the base name used for an entity's handler and test files.
func (e *EntityModel) File() string {
	switch e.Snake {
	case "service", "custom", "acceptance", "fuzz":
		return e.Snake + "_entity"
	}
	return e.Snake
}

// Generate renders all files for the spec.
func Generate(s *spec.Spec) ([]File, error) {
	m := NewModel(s)
	tmpls, err := template.New("").Funcs(funcs).Option("missingkey=error").ParseFS(templateFS, "templates/*.tmpl")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	var files []File
	for _, o := range outputs {
		if o.when != nil && !o.when(m) {
			continue
		}
		targets := []data{{Model: m}}
		if o.perEntity {
			targets = targets[:0]
			for _, e := range m.Entities {
				targets = append(targets, data{Model: m, E: e})
			}
		}
		for _, d := range targets {
			f, err := render(tmpls, o, d)
			if err != nil {
				return nil, err
			}
			files = append(files, f)
		}
	}
	return files, nil
}

func render(tmpls *template.Template, o output, d data) (File, error) {
	var path bytes.Buffer
	pt := template.Must(template.New("path").Parse(o.path))
	if err := pt.Execute(&path, d); err != nil {
		return File{}, fmt.Errorf("path for %s: %w", o.tmpl, err)
	}
	var buf bytes.Buffer
	if err := tmpls.ExecuteTemplate(&buf, o.tmpl, d); err != nil {
		return File{}, fmt.Errorf("render %s: %w", o.tmpl, err)
	}
	out := buf.Bytes()
	if strings.HasSuffix(path.String(), ".go") {
		var err error
		if out, err = tidyGo(out); err != nil {
			return File{}, fmt.Errorf("format %s: %w\n%s", path.String(), err, numbered(buf.Bytes()))
		}
	} else {
		out = collapseBlankLines(out)
	}
	return File{Path: path.String(), Template: o.tmpl, Data: out}, nil
}

// tidyGo removes unused imports and formats Go source.
func tidyGo(src []byte) ([]byte, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok {
				used[id.Name] = true
			}
		}
		return true
	})
	decls := f.Decls[:0]
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			decls = append(decls, decl)
			continue
		}
		specs := gd.Specs[:0]
		for _, sp := range gd.Specs {
			name := importName(sp.(*ast.ImportSpec))
			if name == "_" || name == "." || used[name] {
				specs = append(specs, sp)
			}
		}
		gd.Specs = specs
		if len(specs) > 0 {
			decls = append(decls, gd)
		}
	}
	f.Decls = decls
	f.Imports = slices.DeleteFunc(f.Imports, func(is *ast.ImportSpec) bool {
		n := importName(is)
		return n != "_" && n != "." && !used[n]
	})
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, f); err != nil {
		return nil, err
	}
	return format.Source(collapseBlankLines(buf.Bytes()))
}

var reVersion = regexp.MustCompile(`^v[0-9]+$`)

func importName(is *ast.ImportSpec) string {
	if is.Name != nil {
		return is.Name.Name
	}
	path := strings.Trim(is.Path.Value, `"`)
	parts := strings.Split(path, "/")
	name := parts[len(parts)-1]
	if reVersion.MatchString(name) && len(parts) > 1 {
		name = parts[len(parts)-2]
	}
	return name
}

var reBlankRun = regexp.MustCompile(`\n{3,}`)

func collapseBlankLines(b []byte) []byte {
	return reBlankRun.ReplaceAll(b, []byte("\n\n"))
}

func numbered(b []byte) string {
	var sb strings.Builder
	for i, l := range strings.Split(string(b), "\n") {
		fmt.Fprintf(&sb, "%4d  %s\n", i+1, l)
	}
	return sb.String()
}

// comment wraps text into "// " lines at about 80 columns.
func comment(indent, text string) string {
	var lines []string
	for i, para := range strings.Split(strings.TrimSpace(text), "\n\n") {
		if i > 0 {
			lines = append(lines, indent+"//")
		}
		line := indent + "//"
		for _, w := range strings.Fields(para) {
			if len(line)+1+len(w) > 80 && line != indent+"//" {
				lines = append(lines, line)
				line = indent + "//"
			}
			line += " " + w
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func oneline(text string) string {
	s := strings.Join(strings.Fields(text), " ")
	if s == "" {
		return "Custom method"
	}
	return strings.ReplaceAll(s, "|", `\|`)
}

// WriteFiles writes files below dir, creating directories as needed.
func WriteFiles(dir string, files []File) error {
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil { //nolint:gosec // generated source is meant to be shared
			return err
		}
		if err := os.WriteFile(p, f.Data, 0o644); err != nil { //nolint:gosec // generated source is meant to be shared
			return err
		}
	}
	return nil
}

// Manifest records where each file of a generated service came from.
type Manifest struct {
	Generator   string          `json:"generator"`
	SpecSHA256  string          `json:"spec_sha256"`
	GeneratedAt time.Time       `json:"generated_at"`
	Files       []ManifestEntry `json:"files"`
}

// ManifestEntry is one file in the manifest.
type ManifestEntry struct {
	Path     string `json:"path"`
	Origin   string `json:"origin"`
	Template string `json:"template,omitempty"`
	SHA256   string `json:"sha256"`
}

// ManifestPath is where the manifest lives inside a generated service.
const ManifestPath = ".fliessband/manifest.json"

// NewManifest starts a manifest for the given spec source and files.
func NewManifest(specSrc []byte, files []File) *Manifest {
	m := &Manifest{Generator: "fliessband", SpecSHA256: hash(specSrc), GeneratedAt: time.Now().UTC()}
	for _, f := range files {
		m.Set(f.Path, OriginTemplate, f.Template, f.Data)
	}
	return m
}

// Set records or replaces the entry for path.
func (m *Manifest) Set(path, origin, tmpl string, data []byte) {
	e := ManifestEntry{Path: path, Origin: origin, Template: tmpl, SHA256: hash(data)}
	for i := range m.Files {
		if m.Files[i].Path == path {
			m.Files[i] = e
			return
		}
	}
	m.Files = append(m.Files, e)
	slices.SortFunc(m.Files, func(a, b ManifestEntry) int { return strings.Compare(a.Path, b.Path) })
}

// Origin returns the recorded origin of path, or "" if unknown.
func (m *Manifest) Origin(path string) string {
	for _, f := range m.Files {
		if f.Path == path {
			return f.Origin
		}
	}
	return ""
}

// Save writes the manifest into the service directory.
func (m *Manifest) Save(dir string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(dir, filepath.FromSlash(ManifestPath))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil { //nolint:gosec // part of the generated source tree
		return err
	}
	return os.WriteFile(p, append(b, '\n'), 0o644) //nolint:gosec // part of the generated source tree
}

// LoadManifest reads the manifest from a service directory.
func LoadManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ManifestPath)))
	if err != nil {
		return nil, err
	}
	var m Manifest
	return &m, json.Unmarshal(b, &m)
}

func hash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
