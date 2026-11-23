// Package spec defines the service specification that drives the factory,
// plus parsing, defaulting and validation.
package spec

import (
	"bytes"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Spec describes one microservice.
type Spec struct {
	Name        string   `yaml:"name" json:"name"`
	Module      string   `yaml:"module" json:"module"`
	Package     string   `yaml:"package" json:"package"`
	Description string   `yaml:"description" json:"description"`
	Port        int      `yaml:"port" json:"port"`
	Entities    []Entity `yaml:"entities" json:"entities"`
	Methods     []Method `yaml:"methods" json:"methods"`
	Features    Features `yaml:"features" json:"features"`
	// Migrations controls how schema changes against the previous version
	// are migrated; see migrations.go.
	Migrations Migrations `yaml:"migrations" json:"migrations"`
}

// Entity is a resource with generated CRUD RPCs.
type Entity struct {
	Name   string   `yaml:"name" json:"name"`
	Plural string   `yaml:"plural" json:"plural"`
	Doc    string   `yaml:"doc" json:"doc"`
	Fields []Field  `yaml:"fields" json:"fields"`
	Ops    []string `yaml:"ops" json:"ops"`
	// RenamedFrom is the entity's name in the previous version, so the
	// migration renames its table instead of dropping it.
	RenamedFrom string `yaml:"renamed_from" json:"renamed_from,omitempty"`
}

// Field is a message field. Type is a proto scalar, "timestamp", or (in
// method request/response fields) the name of an entity.
type Field struct {
	Name     string   `yaml:"name" json:"name"`
	Type     string   `yaml:"type" json:"type"`
	Doc      string   `yaml:"doc" json:"doc"`
	Repeated bool     `yaml:"repeated" json:"repeated"`
	Required bool     `yaml:"required" json:"required"`
	Unique   bool     `yaml:"unique" json:"unique"`
	Min      *float64 `yaml:"min" json:"min,omitempty"`
	Max      *float64 `yaml:"max" json:"max,omitempty"`
	MinLen   *int     `yaml:"min_len" json:"min_len,omitempty"`
	MaxLen   *int     `yaml:"max_len" json:"max_len,omitempty"`
	Pattern  string   `yaml:"pattern" json:"pattern,omitempty"`
	Example  string   `yaml:"example" json:"example,omitempty"`
	// RenamedFrom is the field's name in the previous version (entity
	// fields only), so the migration renames its column.
	RenamedFrom string `yaml:"renamed_from" json:"renamed_from,omitempty"`
}

// Method is a custom (non-CRUD) RPC whose behaviour is described in prose
// and pinned down by executable examples.
type Method struct {
	Name     string    `yaml:"name" json:"name"`
	Doc      string    `yaml:"doc" json:"doc"`
	Request  []Field   `yaml:"request" json:"request"`
	Response []Field   `yaml:"response" json:"response"`
	Examples []Example `yaml:"examples" json:"examples,omitempty"`
}

// Example is an acceptance example for a custom method: records to seed, a
// request, and either the expected response fields or an error code. Fliessband
// turns examples into tests that the implementer cannot edit. Holdout
// examples are also hidden from the LLM, so they measure whether the code
// generalises instead of fitting the visible examples.
//
// Strings may refer to seeded records as ${Entity.index.field}, for example
// ${Product.0.id}. Expect keys are dotted paths into the response using proto
// field names; a number selects a list element and # is a list's length.
type Example struct {
	Name    string                      `yaml:"name" json:"name"`
	Given   map[string][]map[string]any `yaml:"given" json:"given,omitempty"`
	Request map[string]any              `yaml:"request" json:"request,omitempty"`
	Expect  map[string]any              `yaml:"expect" json:"expect,omitempty"`
	Error   string                      `yaml:"error" json:"error,omitempty"`
	Holdout bool                        `yaml:"holdout" json:"holdout,omitempty"`
}

// Features toggles optional parts of the generated service.
type Features struct {
	Reflection *bool `yaml:"reflection" json:"reflection"`
	Health     *bool `yaml:"health" json:"health"`
	OTel       bool  `yaml:"otel" json:"otel"`
	Docker     *bool `yaml:"docker" json:"docker"`
	Postgres   *bool `yaml:"postgres" json:"postgres"`
	Kubernetes *bool `yaml:"kubernetes" json:"kubernetes"`
	CI         *bool `yaml:"ci" json:"ci"`
}

func (f Features) HasReflection() bool { return f.Reflection == nil || *f.Reflection }
func (f Features) HasHealth() bool     { return f.Health == nil || *f.Health }
func (f Features) HasDocker() bool     { return f.Docker == nil || *f.Docker }
func (f Features) HasPostgres() bool   { return f.Postgres == nil || *f.Postgres }
func (f Features) HasKubernetes() bool { return f.Kubernetes == nil || *f.Kubernetes }
func (f Features) HasCI() bool         { return f.CI == nil || *f.CI }

// Limits keep a spec within what one factory run can reasonably build.
const (
	MaxEntities = 20
	MaxFields   = 60
	MaxMethods  = 30
	MaxExamples = 20
)

// StatusCodes are the gRPC codes an example may expect, by proto name.
var StatusCodes = []string{"CANCELLED", "UNKNOWN", "INVALID_ARGUMENT", "DEADLINE_EXCEEDED", "NOT_FOUND",
	"ALREADY_EXISTS", "PERMISSION_DENIED", "RESOURCE_EXHAUSTED", "FAILED_PRECONDITION", "ABORTED",
	"OUT_OF_RANGE", "UNIMPLEMENTED", "INTERNAL", "UNAVAILABLE", "DATA_LOSS", "UNAUTHENTICATED"}

// AllOps lists the CRUD operations in canonical order.
var AllOps = []string{"create", "get", "list", "update", "delete"}

// ScalarTypes are the field types allowed on entities.
var ScalarTypes = []string{"string", "bool", "bytes", "int32", "int64", "uint32", "uint64", "float", "double", "timestamp"}

// Problem is one validation finding.
type Problem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
	Warning bool   `json:"warning"`
}

func (p Problem) String() string {
	kind := "error"
	if p.Warning {
		kind = "warning"
	}
	return fmt.Sprintf("%s: %s: %s", kind, p.Path, p.Message)
}

// Parse decodes YAML (or JSON) into a Spec, applies defaults and validates it.
// It returns the problems found; the spec is usable only if none is an error.
func Parse(src []byte) (*Spec, []Problem, error) {
	var s Spec
	dec := yaml.NewDecoder(bytes.NewReader(src))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, nil, fmt.Errorf("parse spec: %w", err)
	}
	s.applyDefaults()
	return &s, s.Validate(), nil
}

// HasErrors reports whether any problem is an error.
func HasErrors(ps []Problem) bool {
	return slices.ContainsFunc(ps, func(p Problem) bool { return !p.Warning })
}

func (s *Spec) applyDefaults() {
	s.Name = strings.TrimSpace(strings.ToLower(s.Name))
	if s.Port == 0 {
		s.Port = 50051
	}
	if s.Module == "" && s.Name != "" {
		s.Module = "example.com/" + s.Name
	}
	if s.Package == "" && s.Name != "" {
		s.Package = strings.ReplaceAll(s.Name, "-", "") + ".v1"
	}
	for i := range s.Entities {
		e := &s.Entities[i]
		if e.Plural == "" {
			e.Plural = Pluralize(e.Name)
		}
		if len(e.Ops) == 0 {
			e.Ops = slices.Clone(AllOps)
		}
	}
}

var (
	reService = regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$`)
	rePascal  = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
	reSnake   = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
	rePackage = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)*\.v[0-9]+((alpha|beta)[0-9]*)?$`)
	reModule  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._~-]*(/[a-zA-Z0-9._~-]+)*$`)
)

var reservedFields = []string{"id", "create_time", "update_time"}

// Validate checks the spec and returns all problems found.
func (s *Spec) Validate() []Problem {
	var ps []Problem
	add := func(path, msg string) { ps = append(ps, Problem{Path: path, Message: msg}) }
	warn := func(path, msg string) { ps = append(ps, Problem{Path: path, Message: msg, Warning: true}) }

	if !reService.MatchString(s.Name) {
		add("name", "use lower-case letters, digits and dashes, e.g. \"inventory\"")
	}
	if !reModule.MatchString(s.Module) {
		add("module", "not a valid Go module path")
	}
	if !rePackage.MatchString(s.Package) {
		add("package", "proto package must be lower-case and end in a version, e.g. \"acme.inventory.v1\"")
	}
	if s.Port < 1 || s.Port > 65535 {
		add("port", "must be between 1 and 65535")
	}
	if len(s.Entities) == 0 && len(s.Methods) == 0 {
		add("entities", "define at least one entity or method")
	}
	if len(s.Entities) > MaxEntities {
		add("entities", fmt.Sprintf("at most %d entities per service; split the domain into several services", MaxEntities))
	}
	if len(s.Methods) > MaxMethods {
		add("methods", fmt.Sprintf("at most %d custom methods per service", MaxMethods))
	}
	if s.Description == "" {
		warn("description", "a one-line description makes the generated docs and proto comments useful")
	}

	names := map[string]string{} // type or RPC name -> where it was defined
	claim := func(name, path string) {
		if prev, ok := names[name]; ok {
			add(path, fmt.Sprintf("%q clashes with %s", name, prev))
			return
		}
		names[name] = path
	}

	for i, e := range s.Entities {
		p := fmt.Sprintf("entities[%d]", i)
		if !rePascal.MatchString(e.Name) {
			add(p+".name", "use PascalCase, e.g. \"Product\"")
			continue
		}
		if !rePascal.MatchString(e.Plural) {
			add(p+".plural", "use PascalCase, e.g. \"Products\"")
		}
		claim(e.Name, p)
		for _, op := range e.Ops {
			if !slices.Contains(AllOps, op) {
				add(p+".ops", fmt.Sprintf("unknown operation %q; use %s", op, strings.Join(AllOps, ", ")))
			}
		}
		for _, rpc := range e.RPCNames() {
			claim(rpc, p+" (RPC)")
		}
		if len(e.Fields) == 0 {
			add(p+".fields", "an entity needs at least one field")
		}
		if len(e.Fields) > MaxFields {
			add(p+".fields", fmt.Sprintf("at most %d fields per entity", MaxFields))
		}
		seen := map[string]bool{}
		for j, f := range e.Fields {
			fp := fmt.Sprintf("%s.fields[%d]", p, j)
			if slices.Contains(reservedFields, f.Name) {
				add(fp+".name", fmt.Sprintf("%q is added automatically", f.Name))
				continue
			}
			if seen[f.Name] {
				add(fp+".name", fmt.Sprintf("duplicate field %q", f.Name))
			}
			seen[f.Name] = true
			ps = append(ps, validateField(fp, f, false, nil)...)
		}
	}

	entities := map[string]bool{}
	for _, e := range s.Entities {
		entities[e.Name] = true
	}
	for i, m := range s.Methods {
		p := fmt.Sprintf("methods[%d]", i)
		if !rePascal.MatchString(m.Name) {
			add(p+".name", "use PascalCase, e.g. \"AdjustStock\"")
			continue
		}
		claim(m.Name, p)
		if strings.TrimSpace(m.Doc) == "" {
			warn(p+".doc", "describe the behaviour: it is the brief for the implementer")
		}
		for _, side := range []struct {
			name   string
			fields []Field
		}{{"request", m.Request}, {"response", m.Response}} {
			seen := map[string]bool{}
			for j, f := range side.fields {
				fp := fmt.Sprintf("%s.%s[%d]", p, side.name, j)
				if seen[f.Name] {
					add(fp+".name", fmt.Sprintf("duplicate field %q", f.Name))
				}
				seen[f.Name] = true
				ps = append(ps, validateField(fp, f, true, entities)...)
			}
		}
		if len(m.Examples) > MaxExamples {
			add(p+".examples", fmt.Sprintf("at most %d examples per method", MaxExamples))
		}
		names := map[string]bool{}
		for j, ex := range m.Examples {
			ep := fmt.Sprintf("%s.examples[%d]", p, j)
			if strings.TrimSpace(ex.Name) == "" {
				add(ep+".name", "give the example a short name, e.g. \"rejects a negative result\"")
			} else if names[ex.Name] {
				add(ep+".name", fmt.Sprintf("duplicate example name %q", ex.Name))
			}
			names[ex.Name] = true
			ps = append(ps, s.validateExample(ep, m, ex)...)
		}
	}
	ps = append(ps, s.validateRenames()...)
	return ps
}

func validateField(p string, f Field, inMethod bool, entities map[string]bool) []Problem {
	var ps []Problem
	add := func(path, msg string) { ps = append(ps, Problem{Path: path, Message: msg}) }
	if !reSnake.MatchString(f.Name) {
		add(p+".name", "use lower_snake_case, e.g. \"price_cents\"")
	}
	isEntity := inMethod && entities[f.Type]
	if !slices.Contains(ScalarTypes, f.Type) && !isEntity {
		allowed := strings.Join(ScalarTypes, ", ")
		if inMethod {
			allowed += ", or an entity name"
		}
		add(p+".type", fmt.Sprintf("unknown type %q; use %s", f.Type, allowed))
		return ps
	}
	if inMethod {
		// Constraints on custom RPC fields are left to the implementer.
		return ps
	}
	numeric := IsNumeric(f.Type)
	if f.Required && (numeric || f.Type == "bool") && !f.Repeated {
		add(p+".required", fmt.Sprintf("proto3 cannot tell an unset %s from zero; use min instead", f.Type))
	}
	if (f.Min != nil || f.Max != nil) && (!numeric || f.Repeated) {
		add(p, "min and max apply to single numeric fields only")
	}
	if f.Min != nil && f.Max != nil && *f.Min > *f.Max {
		add(p, "min is greater than max")
	}
	if f.Min != nil && *f.Min < 0 && strings.HasPrefix(f.Type, "uint") {
		add(p+".min", "unsigned fields cannot be negative")
	}
	for _, b := range []struct {
		name string
		v    *float64
	}{{"min", f.Min}, {"max", f.Max}} {
		if b.v == nil || !IsInteger(f.Type) {
			continue
		}
		lo, hi := intRange(f.Type)
		if *b.v != math.Trunc(*b.v) || *b.v < lo || *b.v > hi {
			add(p+"."+b.name, fmt.Sprintf("must be a whole number that fits in %s", f.Type))
		}
	}
	if (f.MinLen != nil || f.MaxLen != nil || f.Pattern != "") && (f.Type != "string" || f.Repeated) {
		add(p, "min_len, max_len and pattern apply to single string fields only")
	}
	if f.MinLen != nil && f.MaxLen != nil && *f.MinLen > *f.MaxLen {
		add(p, "min_len is greater than max_len")
	}
	if f.Unique && (f.Type != "string" || f.Repeated) {
		add(p+".unique", "unique applies to single string fields only")
	}
	if f.Pattern != "" {
		re, err := regexp.Compile(f.Pattern)
		if err != nil {
			add(p+".pattern", "invalid regular expression: "+err.Error())
			return ps
		}
		if f.Example == "" {
			add(p+".example", "a field with a pattern needs an example value that matches it (used in generated tests)")
			return ps
		}
		if !re.MatchString(f.Example) {
			add(p+".example", fmt.Sprintf("example %q does not match pattern", f.Example))
		}
		if f.Unique {
			for i := range 10 {
				if v := Variant(f.Example, i); !re.MatchString(v) {
					add(p+".example", fmt.Sprintf("unique field: variant %q of the example does not match the pattern; end the example in digits, e.g. \"SKU-001\"", v))
					break
				}
			}
		}
	}
	if f.Example != "" && f.Type == "string" {
		n := len([]rune(f.Example))
		if (f.MinLen != nil && n < *f.MinLen) || (f.MaxLen != nil && n > *f.MaxLen) {
			add(p+".example", "example length is outside min_len/max_len")
		}
	}
	return ps
}

// IsNumeric reports whether t is a numeric proto scalar.
func IsNumeric(t string) bool {
	switch t {
	case "int32", "int64", "uint32", "uint64", "float", "double":
		return true
	}
	return false
}

func intRange(t string) (lo, hi float64) {
	switch t {
	case "int32":
		return math.MinInt32, math.MaxInt32
	case "uint32":
		return 0, math.MaxUint32
	case "uint64":
		return 0, 1 << 53 // keep bounds exactly representable as float64
	}
	return -(1 << 53), 1 << 53
}

// IsInteger reports whether t is an integer proto scalar.
func IsInteger(t string) bool {
	return IsNumeric(t) && t != "float" && t != "double"
}

// HasOp reports whether the entity exposes the given CRUD operation.
func (e Entity) HasOp(op string) bool { return slices.Contains(e.Ops, op) }

// RPCNames returns the names of the entity's CRUD RPCs.
func (e Entity) RPCNames() []string {
	var out []string
	for _, op := range AllOps {
		if !e.HasOp(op) {
			continue
		}
		if op == "list" {
			out = append(out, "List"+e.Plural)
		} else {
			out = append(out, strings.ToUpper(op[:1])+op[1:]+e.Name)
		}
	}
	return out
}

// RPCCount returns the number of RPCs the service exposes.
func (s *Spec) RPCCount() int {
	n := len(s.Methods)
	for _, e := range s.Entities {
		n += len(e.Ops)
	}
	return n
}

// Variant returns the i-th distinct value derived from an example string,
// replacing trailing digits (keeping their width) or appending a number.
func Variant(example string, i int) string {
	j := len(example)
	for j > 0 && example[j-1] >= '0' && example[j-1] <= '9' {
		j--
	}
	digits := len(example) - j
	if digits == 0 {
		return fmt.Sprintf("%s%d", example, i)
	}
	var base int
	_, _ = fmt.Sscanf(example[j:], "%d", &base)
	return fmt.Sprintf("%s%0*d", example[:j], digits, base+i)
}

// Pluralize returns a simple English plural for a PascalCase noun.
func Pluralize(s string) string {
	switch {
	case s == "":
		return s
	case strings.HasSuffix(s, "y") && len(s) > 1 && !strings.ContainsRune("aeiou", rune(s[len(s)-2])):
		return s[:len(s)-1] + "ies"
	case strings.HasSuffix(s, "s"), strings.HasSuffix(s, "x"), strings.HasSuffix(s, "z"),
		strings.HasSuffix(s, "ch"), strings.HasSuffix(s, "sh"):
		return s + "es"
	}
	return s + "s"
}
