package gen

import (
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/tillknuesting/fliessband/internal/spec"
)

// ColumnModel maps an entity field to a PostgreSQL column and back.
type ColumnModel struct {
	Ident   string // quoted SQL identifier
	DDL     string // column definition inside CREATE TABLE
	ScanVar string // Go type of the scan target
	Arg     string // Go expression for the query argument (the record is x)
	Assign  string // Go statement copying the scan target cGoName into x
}

// UniqueIndex is a partial unique index for a unique field.
type UniqueIndex struct {
	Name   string // index (constraint) name reported in unique violations
	Column string
	Field  *FieldModel
}

// sqlElem is the PostgreSQL element type and Go scan type of a proto scalar.
func sqlElem(t string) (sqlType, goType string) {
	switch t {
	case "string":
		return "text", "string"
	case "bool":
		return "boolean", "bool"
	case "bytes":
		return "bytea", "[]byte"
	case "int32":
		return "integer", "int32"
	case "int64":
		return "bigint", "int64"
	case "uint32":
		return "bigint", "int64"
	case "uint64":
		return "numeric(20,0)", "uint64"
	case "float":
		return "real", "float32"
	case "double":
		return "double precision", "float64"
	case "timestamp":
		return "timestamptz", "time.Time"
	}
	panic("no SQL type for " + t)
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// newColumn derives the column for f and records the helpers it needs.
func newColumn(f *FieldModel, helpers map[string]bool) *ColumnModel {
	c := &ColumnModel{Ident: quoteIdent(f.Name)}
	sqlType, goType := sqlElem(f.Type)
	get := "x.Get" + f.GoName + "()"
	dst := "x." + f.GoName
	src := "c" + f.GoName
	var checks []string
	switch {
	case f.Repeated:
		c.DDL = fmt.Sprintf("%s %s[] NOT NULL DEFAULT '{}'", c.Ident, sqlType)
		c.ScanVar = "[]" + goType
		c.Arg = "orEmpty(" + get + ")"
		c.Assign = dst + " = " + src
		helpers["orEmpty"] = true
		switch f.Type {
		case "uint32":
			c.Arg, c.Assign = "uint32sToInt64s("+get+")", dst+" = int64sToUint32s("+src+")"
			helpers["uint32s"] = true
		case "timestamp":
			c.Arg, c.Assign = "timesFromProto("+get+")", dst+" = timesToProto("+src+")"
			helpers["times"] = true
		}
		if f.Required {
			checks = append(checks, fmt.Sprintf("cardinality(%s) > 0", c.Ident))
		}
	case f.Type == "timestamp":
		c.DDL = c.Ident + " timestamptz"
		c.ScanVar = "*time.Time"
		c.Arg, c.Assign = "timeOrNil("+get+")", dst+" = timeToProto("+src+")"
		helpers["time"] = true
		if f.Required {
			checks = append(checks, c.Ident+" IS NOT NULL")
		}
	default:
		def := "0"
		switch f.Type {
		case "string":
			def = "''"
		case "bool":
			def = "false"
		case "bytes":
			def = "''::bytea"
		}
		c.DDL = fmt.Sprintf("%s %s NOT NULL DEFAULT %s", c.Ident, sqlType, def)
		c.ScanVar = goType
		c.Arg, c.Assign = get, dst+" = "+src
		switch f.Type {
		case "bytes":
			c.Arg = "orEmpty(" + get + ")"
			helpers["orEmpty"] = true
		case "uint32":
			c.Arg, c.Assign = "int64("+get+")", dst+" = uint32(min(max("+src+", 0), math.MaxUint32))"
			checks = append(checks, c.Ident+" BETWEEN 0 AND 4294967295")
		case "uint64":
			checks = append(checks, c.Ident+" >= 0")
		}
		checks = append(checks, valueChecks(f, c.Ident)...)
	}
	for _, ch := range checks {
		c.DDL += " CHECK (" + ch + ")"
	}
	return c
}

// valueChecks mirrors the spec's validation rules as CHECK constraints, so
// the database rejects bad rows even if a code path skips validation.
func valueChecks(f *FieldModel, col string) []string {
	var out []string
	switch {
	case f.Type == "string":
		var length string
		switch {
		case f.MinLen != nil && f.MaxLen != nil:
			length = fmt.Sprintf("char_length(%s) BETWEEN %d AND %d", col, *f.MinLen, *f.MaxLen)
		case f.MinLen != nil:
			length = fmt.Sprintf("char_length(%s) >= %d", col, *f.MinLen)
		case f.MaxLen != nil:
			length = fmt.Sprintf("char_length(%s) <= %d", col, *f.MaxLen)
		}
		switch {
		case f.Required && length != "":
			out = append(out, col+" <> ''", length)
		case f.Required:
			out = append(out, col+" <> ''")
		case length != "":
			out = append(out, col+" = '' OR "+length)
		}
	case spec.IsNumeric(f.Type):
		unsigned := strings.HasPrefix(f.Type, "uint")
		switch {
		case f.Min != nil && f.Max != nil:
			out = append(out, fmt.Sprintf("%s BETWEEN %s AND %s", col, num(f.Type, *f.Min), num(f.Type, *f.Max)))
		case f.Min != nil && !(unsigned && *f.Min <= 0):
			out = append(out, fmt.Sprintf("%s >= %s", col, num(f.Type, *f.Min)))
		case f.Max != nil:
			out = append(out, fmt.Sprintf("%s <= %s", col, num(f.Type, *f.Max)))
		}
	}
	return out
}

// sqlModel fills the PostgreSQL view of an entity.
func (em *EntityModel) sqlModel(helpers map[string]bool) {
	em.Table = quoteIdent(em.PluralSnake)
	em.TableName = em.PluralSnake
	cols := []string{`"id"`}
	for _, f := range em.Fields {
		f.Col = newColumn(f, helpers)
		cols = append(cols, f.Col.Ident)
		if f.Unique {
			em.UniqueIndexes = append(em.UniqueIndexes, UniqueIndex{
				Name:   em.PluralSnake + "_" + f.Name + "_key",
				Column: f.Col.Ident,
				Field:  f,
			})
		}
	}
	cols = append(cols, `"create_time"`, `"update_time"`)
	em.SQLColumns = strings.Join(cols, ", ")
	ph := make([]string, len(cols))
	for i := range cols {
		ph[i] = fmt.Sprintf("$%d", i+1)
	}
	n := len(cols)
	ph[n-2] = fmt.Sprintf("COALESCE($%d, now())", n-1)
	ph[n-1] = fmt.Sprintf("COALESCE($%d, now())", n)
	em.SQLPlaceholders = strings.Join(ph, ", ")
	var sets []string
	for i, c := range cols[1 : n-2] {
		sets = append(sets, fmt.Sprintf("%s = $%d", c, i+2))
	}
	sets = append(sets, fmt.Sprintf(`"create_time" = COALESCE($%d, "create_time")`, n-1), fmt.Sprintf(`"update_time" = COALESCE($%d, now())`, n))
	em.SQLSets = strings.Join(sets, ", ")
}

// lockKey is the advisory lock id that serialises migrations of a service.
func lockKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("fliessband-migrate:" + name))
	return int64(h.Sum64() & 0x7fffffffffffffff)
}
