package gen

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/tillknuesting/fliessband/internal/spec"
)

// Model is the view of a spec that templates render.
type Model struct {
	Spec            *spec.Spec
	Name            string // inventory
	Title           string // Inventory
	Module          string
	Description     string
	ProtoPackage    string // acme.inventory.v1
	ProtoDir        string // acme/inventory/v1
	ProtoFile       string // inventory.proto
	GoPkgName       string // inventoryv1
	GoPkgPath       string // <module>/gen/acme/inventory/v1
	ServiceName     string // InventoryService
	FullServiceName string // acme.inventory.v1.InventoryService
	Port            int
	GoVersion       string // go directive in go.mod
	GoMinor         string // golang image tag: the minor version, so patch releases apply
	Entities        []*EntityModel
	Methods         []*MethodModel
	Examples        []*ExampleModel
	OTel            bool
	Health          bool
	Reflection      bool
	Docker          bool
	Postgres        bool
	Kubernetes      bool
	CI              bool
	LockKey         int64           // advisory lock id for migrations
	PGHelpers       map[string]bool // conversion helpers the Postgres store needs
}

// EntityModel is an entity with derived names and test data.
type EntityModel struct {
	Name, Plural, Doc string
	Lower             string // "order line"
	PluralLower       string // "order lines"
	Snake             string // order_line (request field name)
	PluralSnake       string // order_lines
	GoField           string // OrderLine (Go field for Snake)
	GoPluralField     string // OrderLines
	Var               string // orderLine
	PluralVar         string // orderLines
	Fields            []*FieldModel
	Unique            []*FieldModel
	HasCreate         bool
	HasGet            bool
	HasList           bool
	HasUpdate         bool
	HasDelete         bool
	CreateNum         int
	UpdateNum         int
	Patterns          []PatternVar
	Invalid           []InvalidCase
	Boundary          []InvalidCase // valid values exactly at a limit
	Update            *UpdateCase
	ExampleJSON       string

	// PostgreSQL
	Table           string // quoted table name
	TableName       string
	SQLColumns      string // quoted column list: id, fields, create_time, update_time
	SQLPlaceholders string // VALUES list matching SQLColumns
	SQLSets         string // SET list for UPDATE; $1 is the id
	UniqueIndexes   []UniqueIndex
}

// FieldModel is a field with derived Go/proto names and test values.
type FieldModel struct {
	spec.Field
	GoName     string
	ProtoType  string
	Number     int
	DocLines   string // proto comment text
	Validation string // Go statements validating m.<field>
	ValidExpr  string // Go expression of a valid value; may use i for unique fields
	IsEntity   bool
	Col        *ColumnModel // PostgreSQL column (entity fields only)
}

// PatternVar is a package-level compiled regexp for a field pattern.
type PatternVar struct{ Var, Literal string }

// InvalidCase is one generated validation test.
type InvalidCase struct {
	Name, Field, GoName, Expr string
}

// UpdateCase drives the generated field-mask update test.
type UpdateCase struct {
	Field  *FieldModel
	Expr   string
	Equals bool // value can be compared with ==
}

// MethodModel is a custom RPC.
type MethodModel struct {
	Name     string
	Doc      string
	Request  []*FieldModel
	Response []*FieldModel
}

// GoVersion is the go directive of generated modules. Go 1.27 brings the
// uuid package (UUIDv7 ids) and the goroutine leak profile used in tests.
const GoVersion = "1.27"

// NewModel derives the template model from a validated spec.
func NewModel(s *spec.Spec) *Model {
	segs := strings.Split(s.Package, ".")
	ver := segs[len(segs)-1]
	base := segs[len(segs)-2]
	svc := strings.TrimSuffix(pascal(s.Name), "Service") + "Service"
	if svc == "Service" {
		svc = pascal(base) + "Service"
	}
	m := &Model{
		Spec:            s,
		Name:            s.Name,
		Title:           pascal(s.Name),
		Module:          s.Module,
		Description:     strings.TrimSpace(s.Description),
		ProtoPackage:    s.Package,
		ProtoDir:        strings.ReplaceAll(s.Package, ".", "/"),
		ProtoFile:       strings.ReplaceAll(s.Name, "-", "_") + ".proto",
		GoPkgName:       base + ver,
		GoPkgPath:       s.Module + "/gen/" + strings.ReplaceAll(s.Package, ".", "/"),
		ServiceName:     svc,
		FullServiceName: s.Package + "." + svc,
		Port:            s.Port,
		GoVersion:       GoVersion,
		GoMinor:         GoVersion,
		OTel:            s.Features.OTel,
		Health:          s.Features.HasHealth(),
		Reflection:      s.Features.HasReflection(),
		Docker:          s.Features.HasDocker(),
		Postgres:        s.Features.HasPostgres() && len(s.Entities) > 0,
		Kubernetes:      s.Features.HasKubernetes(),
		CI:              s.Features.HasCI(),
		LockKey:         lockKey(s.Name),
		PGHelpers:       map[string]bool{},
	}
	for _, e := range s.Entities {
		em := newEntity(e)
		em.sqlModel(m.PGHelpers)
		m.Entities = append(m.Entities, em)
	}
	m.Examples = newExamples(s)
	for _, sm := range s.Methods {
		mm := &MethodModel{Name: sm.Name, Doc: strings.TrimSpace(sm.Doc)}
		for i, f := range sm.Request {
			mm.Request = append(mm.Request, newField(f, i+1, "", s))
		}
		for i, f := range sm.Response {
			mm.Response = append(mm.Response, newField(f, i+1, "", s))
		}
		m.Methods = append(m.Methods, mm)
	}
	return m
}

func newEntity(e spec.Entity) *EntityModel {
	em := &EntityModel{
		Name:        e.Name,
		Plural:      e.Plural,
		Doc:         strings.TrimSpace(e.Doc),
		Lower:       words(e.Name),
		PluralLower: words(e.Plural),
		Snake:       snake(e.Name),
		PluralSnake: snake(e.Plural),
		Var:         safeVar(e.Name),
		PluralVar:   safeVar(e.Plural),
		HasCreate:   e.HasOp("create"),
		HasGet:      e.HasOp("get"),
		HasList:     e.HasOp("list"),
		HasUpdate:   e.HasOp("update"),
		HasDelete:   e.HasOp("delete"),
	}
	em.GoField = goCamelCase(em.Snake)
	em.GoPluralField = goCamelCase(em.PluralSnake)
	for i, f := range e.Fields {
		fm := newField(f, i+2, em.Var+goCamelCase(f.Name)+"Pattern", nil)
		em.Fields = append(em.Fields, fm)
		if f.Unique {
			em.Unique = append(em.Unique, fm)
		}
		if f.Pattern != "" {
			em.Patterns = append(em.Patterns, PatternVar{Var: em.Var + fm.GoName + "Pattern", Literal: goStringLit(f.Pattern)})
		}
		em.Invalid = append(em.Invalid, invalidCases(fm)...)
		em.Boundary = append(em.Boundary, boundaryCases(fm)...)
	}
	em.CreateNum = len(e.Fields) + 2
	em.UpdateNum = len(e.Fields) + 3
	em.Update = pickUpdate(em.Fields)
	em.ExampleJSON = exampleJSON(em)
	return em
}

func newField(f spec.Field, num int, patternVar string, s *spec.Spec) *FieldModel {
	fm := &FieldModel{Field: f, GoName: goCamelCase(f.Name), Number: num}
	switch f.Type {
	case "timestamp":
		fm.ProtoType = "google.protobuf.Timestamp"
	default:
		fm.ProtoType = f.Type
		fm.IsEntity = !slices.Contains(spec.ScalarTypes, f.Type)
	}
	fm.DocLines = fieldDoc(f)
	if s == nil { // entity field
		fm.Validation = validation(fm, patternVar)
		fm.ValidExpr = validExpr(f)
	}
	return fm
}

// goStringLit prefers a raw string literal, which reads better for regexps.
func goStringLit(s string) string {
	if !strings.ContainsAny(s, "`\r") && strconv.CanBackquote(s) {
		return "`" + s + "`"
	}
	return strconv.Quote(s)
}

// fieldDoc combines the user's doc with a description of the rules.
func fieldDoc(f spec.Field) string {
	var parts []string
	if d := strings.TrimSpace(f.Doc); d != "" {
		parts = append(parts, strings.TrimSuffix(d, ".")+".")
	}
	if f.Required {
		parts = append(parts, "Required.")
	}
	if f.Unique {
		parts = append(parts, "Unique.")
	}
	switch {
	case f.MinLen != nil && f.MaxLen != nil:
		parts = append(parts, fmt.Sprintf("%d to %d characters.", *f.MinLen, *f.MaxLen))
	case f.MinLen != nil:
		parts = append(parts, fmt.Sprintf("At least %d characters.", *f.MinLen))
	case f.MaxLen != nil:
		parts = append(parts, fmt.Sprintf("At most %d characters.", *f.MaxLen))
	}
	if f.Min != nil {
		parts = append(parts, fmt.Sprintf("Minimum %s.", num(f.Type, *f.Min)))
	}
	if f.Max != nil {
		parts = append(parts, fmt.Sprintf("Maximum %s.", num(f.Type, *f.Max)))
	}
	if f.Pattern != "" {
		parts = append(parts, fmt.Sprintf("Must match %s.", f.Pattern))
	}
	if len(parts) == 0 {
		return "The " + strings.ReplaceAll(f.Name, "_", " ") + "."
	}
	return strings.Join(parts, " ")
}

// num formats a bound as a Go constant of the field's type.
func num(typ string, v float64) string {
	if spec.IsInteger(typ) {
		return strconv.FormatInt(int64(v), 10)
	}
	s := strconv.FormatFloat(v, 'g', -1, 64)
	if !strings.ContainsAny(s, ".e") {
		s += ".0"
	}
	return s
}

// validation returns Go statements that append violations for m's field.
func validation(f *FieldModel, patternVar string) string {
	get := "m.Get" + f.GoName + "()"
	path := `prefix+"` + f.Name + `"`
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	switch {
	case f.Repeated || f.Type == "bytes":
		if f.Required {
			w("if len(%s) == 0 {\n\tv.add(%s, \"must not be empty\")\n}\n", get, path)
		}
	case f.Type == "timestamp":
		if f.Required {
			w("if %s == nil {\n\tv.add(%s, \"is required\")\n}\n", get, path)
		}
	case f.Type == "string":
		var checks strings.Builder
		if f.MinLen != nil || f.MaxLen != nil {
			checks.WriteString("n := utf8.RuneCountInString(s)\n")
		}
		if f.MinLen != nil {
			fmt.Fprintf(&checks, "if n < %d {\n\tv.add(%s, \"must be at least %d characters\")\n}\n", *f.MinLen, path, *f.MinLen)
		}
		if f.MaxLen != nil {
			fmt.Fprintf(&checks, "if n > %d {\n\tv.add(%s, \"must be at most %d characters\")\n}\n", *f.MaxLen, path, *f.MaxLen)
		}
		if f.Pattern != "" {
			fmt.Fprintf(&checks, "if !%s.MatchString(s) {\n\tv.add(%s, %s)\n}\n", patternVar, path, strconv.Quote("must match "+f.Pattern))
		}
		switch {
		case f.Required && checks.Len() > 0:
			w("if s := %s; s == \"\" {\n\tv.add(%s, \"is required\")\n} else {\n%s}\n", get, path, checks.String())
		case f.Required:
			w("if %s == \"\" {\n\tv.add(%s, \"is required\")\n}\n", get, path)
		case checks.Len() > 0:
			w("if s := %s; s != \"\" {\n%s}\n", get, checks.String())
		}
	case spec.IsNumeric(f.Type):
		unsigned := strings.HasPrefix(f.Type, "uint")
		if f.Min != nil && !(unsigned && *f.Min <= 0) {
			w("if %s < %s {\n\tv.add(%s, \"must be at least %s\")\n}\n", get, num(f.Type, *f.Min), path, num(f.Type, *f.Min))
		}
		if f.Max != nil {
			w("if %s > %s {\n\tv.add(%s, \"must be at most %s\")\n}\n", get, num(f.Type, *f.Max), path, num(f.Type, *f.Max))
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// defaultString builds a valid string value for f, honouring its length rules.
func defaultString(f spec.Field) string {
	if f.Example != "" {
		return f.Example
	}
	base := strings.ReplaceAll(f.Name, "_", "-")
	suffix := ""
	if f.Unique {
		suffix = "-001"
	}
	if f.MinLen != nil {
		for len([]rune(base+suffix)) < *f.MinLen {
			base += "x"
		}
	}
	if f.MaxLen != nil {
		for len(base) > 0 && len([]rune(base+suffix)) > *f.MaxLen {
			base = base[:len(base)-1]
		}
		if len([]rune(base+suffix)) > *f.MaxLen {
			suffix = suffix[len(suffix)-*f.MaxLen:]
		}
	}
	return base + suffix
}

func validNumber(f spec.Field) float64 {
	v := 1.0
	if f.Min != nil {
		v = *f.Min
	}
	if f.Max != nil && v > *f.Max {
		v = *f.Max
	}
	return v
}

// validExpr is a Go expression for a valid value of f. Unique string fields
// vary with the loop variable i so tests can create several records.
func validExpr(f spec.Field) string {
	var elem string
	switch f.Type {
	case "string":
		s := defaultString(f)
		if f.Unique && !f.Repeated {
			return uniqueExpr(s)
		}
		elem = strconv.Quote(s)
	case "bool":
		elem = "true"
	case "bytes":
		if f.Repeated {
			return `[][]byte{[]byte("data")}`
		}
		return `[]byte("data")`
	case "timestamp":
		elem = "timestamppb.New(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))"
	default:
		elem = num(f.Type, validNumber(f))
	}
	if f.Repeated {
		return "[]" + goElemType(f.Type) + "{" + elem + "}"
	}
	return elem
}

// uniqueExpr mirrors spec.Variant: trailing digits are replaced, keeping width.
func uniqueExpr(example string) string {
	j := len(example)
	for j > 0 && isDigit(example[j-1]) {
		j--
	}
	digits := len(example) - j
	prefix := strings.ReplaceAll(example[:j], "%", "%%")
	if digits == 0 {
		return fmt.Sprintf("fmt.Sprintf(%s, i)", strconv.Quote(prefix+"%d"))
	}
	base, _ := strconv.Atoi(example[j:])
	return fmt.Sprintf("fmt.Sprintf(%s, %d+i)", strconv.Quote(fmt.Sprintf("%s%%0%dd", prefix, digits)), base)
}

func goElemType(t string) string {
	switch t {
	case "double":
		return "float64"
	case "float":
		return "float32"
	case "bytes":
		return "[]byte"
	case "timestamp":
		return "*timestamppb.Timestamp"
	}
	return t
}

// invalidCases lists values that must fail validation, one test each.
func invalidCases(f *FieldModel) []InvalidCase {
	var out []InvalidCase
	add := func(name, expr string) {
		out = append(out, InvalidCase{Name: f.Name + " " + name, Field: f.Name, GoName: f.GoName, Expr: expr})
	}
	switch {
	case f.Repeated || f.Type == "bytes" || f.Type == "timestamp":
		if f.Required {
			add("missing", "nil")
		}
	case f.Type == "string":
		if f.Required {
			add("missing", `""`)
		}
		if f.MinLen != nil && *f.MinLen > 1 {
			add("too short", strconv.Quote(strings.Repeat("a", *f.MinLen-1)))
		}
		if f.MaxLen != nil {
			add("too long", fmt.Sprintf("strings.Repeat(%q, %d)", "a", *f.MaxLen+1))
		}
		if f.Pattern != "" {
			re := regexp.MustCompile(f.Pattern)
			for _, cand := range []string{"!", " ", "~~~", "invalid value", "0"} {
				if !re.MatchString(cand) {
					add("pattern", strconv.Quote(cand))
					break
				}
			}
		}
	case spec.IsNumeric(f.Type):
		unsigned := strings.HasPrefix(f.Type, "uint")
		if f.Min != nil && !(unsigned && *f.Min <= 0) {
			add("below min", num(f.Type, *f.Min-1))
		}
		if f.Max != nil {
			add("above max", num(f.Type, *f.Max+1))
		}
	}
	return out
}

// boundaryCases lists values exactly at each limit, which must be accepted.
func boundaryCases(f *FieldModel) []InvalidCase {
	var out []InvalidCase
	add := func(name, expr string) {
		out = append(out, InvalidCase{Name: f.Name + " " + name, Field: f.Name, GoName: f.GoName, Expr: expr})
	}
	switch {
	case f.Repeated:
	case f.Type == "string" && f.Pattern == "":
		if f.MinLen != nil && *f.MinLen > 0 {
			add("at min_len", fmt.Sprintf("strings.Repeat(%q, %d)", "a", *f.MinLen))
		}
		if f.MaxLen != nil && (f.MinLen == nil || *f.MaxLen != *f.MinLen) {
			add("at max_len", fmt.Sprintf("strings.Repeat(%q, %d)", "b", *f.MaxLen))
		}
	case spec.IsNumeric(f.Type):
		unsigned := strings.HasPrefix(f.Type, "uint")
		if f.Min != nil && !(unsigned && *f.Min <= 0) {
			add("at min", num(f.Type, *f.Min))
		}
		if f.Max != nil && (f.Min == nil || *f.Max != *f.Min) {
			add("at max", num(f.Type, *f.Max))
		}
	}
	return out
}

// pickUpdate chooses a field and a new valid value for the field-mask test.
func pickUpdate(fields []*FieldModel) *UpdateCase {
	for _, f := range fields {
		if f.Type == "bool" && !f.Repeated {
			return &UpdateCase{Field: f, Expr: "false", Equals: true}
		}
	}
	for _, f := range fields {
		if spec.IsNumeric(f.Type) && !f.Repeated {
			v := validNumber(f.Field) + 1
			if f.Max == nil || v <= *f.Max {
				return &UpdateCase{Field: f, Expr: num(f.Type, v), Equals: true}
			}
		}
	}
	for _, f := range fields {
		if f.Type == "string" && !f.Repeated && f.Pattern == "" && !f.Unique {
			g := f.Field
			g.Example = ""
			g.Name = "updated_" + f.Name
			return &UpdateCase{Field: f, Expr: strconv.Quote(defaultString(g)), Equals: true}
		}
	}
	for _, f := range fields {
		if f.Type == "string" && !f.Repeated && f.Unique {
			return &UpdateCase{Field: f, Expr: strconv.Quote(spec.Variant(defaultString(f.Field), 9)), Equals: true}
		}
	}
	if len(fields) > 0 {
		return &UpdateCase{Field: fields[0], Expr: fields[0].ValidExpr}
	}
	return nil
}

// exampleJSON is a request body for grpcurl examples in the README.
func exampleJSON(e *EntityModel) string {
	obj := map[string]any{}
	for _, f := range e.Fields {
		var v any
		switch f.Type {
		case "string":
			s := defaultString(f.Field)
			v = s
		case "bool":
			v = true
		case "bytes":
			v = base64.StdEncoding.EncodeToString([]byte("data"))
		case "timestamp":
			v = "2026-01-02T03:04:05Z"
		default:
			v = validNumber(f.Field)
		}
		if f.Repeated {
			v = []any{v}
		}
		obj[f.Name] = v
	}
	b, _ := json.Marshal(map[string]any{e.Snake: obj})
	return string(b)
}

// AnyList reports whether any entity has a List RPC (paging helpers needed).
func (m *Model) AnyList() bool {
	for _, e := range m.Entities {
		if e.HasList {
			return true
		}
	}
	return false
}

// AnyValidate reports whether any entity validates input.
func (m *Model) AnyValidate() bool {
	for _, e := range m.Entities {
		if e.HasCreate || e.HasUpdate {
			return true
		}
	}
	return false
}

// AnyCreate reports whether ids are generated.
func (m *Model) AnyCreate() bool {
	for _, e := range m.Entities {
		if e.HasCreate {
			return true
		}
	}
	return false
}

// NeedsFieldMask reports whether the proto imports field_mask.proto.
func (m *Model) NeedsFieldMask() bool {
	for _, e := range m.Entities {
		if e.HasUpdate {
			return true
		}
	}
	return false
}

// NeedsTimestamp reports whether the proto imports timestamp.proto.
func (m *Model) NeedsTimestamp() bool {
	if len(m.Entities) > 0 {
		return true
	}
	for _, mm := range m.Methods {
		for _, f := range append(append([]*FieldModel{}, mm.Request...), mm.Response...) {
			if f.Type == "timestamp" {
				return true
			}
		}
	}
	return false
}

// HasRules reports whether the entity has any validation rules.
func (e *EntityModel) HasRules() bool {
	for _, f := range e.Fields {
		if f.Validation != "" {
			return true
		}
	}
	return false
}
