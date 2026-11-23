package gen

import (
	"bytes"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/tillknuesting/fliessband/internal/spec"
)

// MigrationsDir holds a generated service's SQL migrations, applied in file
// name order by store.OpenPostgres.
const MigrationsDir = "internal/store/migrations"

// Kinds of schema change between two versions of a service.
const (
	ChangeCreateTable  = "create_table"
	ChangeDropTable    = "drop_table"
	ChangeRenameTable  = "rename_table"
	ChangeAddColumn    = "add_column"
	ChangeDropColumn   = "drop_column"
	ChangeRenameColumn = "rename_column"
	ChangeAlterType    = "alter_type"
	ChangeAlterCheck   = "alter_check"
	ChangeAddUnique    = "add_unique"
	ChangeDropUnique   = "drop_unique"
)

// MigrationChange is one difference between the database schema of the
// previous version and the schema this spec needs.
type MigrationChange struct {
	Kind   string
	Table  string // table name in the new schema (the old one for drops)
	Column string
	Detail string // one line for people
	// Destructive changes can lose data or fail on existing rows. They stop
	// the line unless the spec sets migrations.allow_destructive.
	Destructive bool
	SQL         []string
}

// MigrationPlan is the set of changes from one version's schema to the next.
type MigrationPlan struct {
	Changes          []MigrationChange
	AllowDestructive bool
}

// Destructive returns the changes that need migrations.allow_destructive.
func (p *MigrationPlan) Destructive() []MigrationChange {
	var out []MigrationChange
	for _, c := range p.Changes {
		if c.Destructive {
			out = append(out, c)
		}
	}
	return out
}

// Blocked reports whether the plan has destructive changes the spec does
// not allow.
func (p *MigrationPlan) Blocked() bool {
	return !p.AllowDestructive && len(p.Destructive()) > 0
}

// PlanMigration compares the PostgreSQL schema the generator emits for prev
// with the one it emits for next. Entities are matched by name, or by
// renamed_from; fields likewise. Both specs must be valid.
//
// The rules:
//   - Safe: new tables; new columns whose zero value satisfies their
//     constraints (columns are added with the generator's defaults, so
//     existing rows read as the proto3 zero value, exactly as an older client
//     would see them); renames declared with renamed_from (or a changed
//     plural); dropping a unique index; loosening CHECK constraints;
//     widening a type (integer to bigint, numeric or double precision,
//     bigint to numeric, real to double precision).
//   - Destructive: dropping a table or column; any other type change; a
//     tighter CHECK or a new required column, which existing rows may
//     violate; a new unique index, which existing duplicates make fail.
//
// Tighter constraints are added NOT VALID when allowed: PostgreSQL enforces
// them for new and updated rows and leaves existing rows to a later
// VALIDATE CONSTRAINT.
func PlanMigration(prev, next *spec.Spec) *MigrationPlan {
	pm, nm := NewModel(prev), NewModel(next)
	plan := &MigrationPlan{AllowDestructive: next.Migrations.AllowDestructive}
	var drops, renames, creates, tables []MigrationChange

	oldByName := map[string]*EntityModel{}
	for _, e := range pm.Entities {
		oldByName[e.Name] = e
	}
	newNames := map[string]bool{}
	for _, e := range next.Entities {
		newNames[e.Name] = true
	}
	matched := map[string]bool{}
	for i, ne := range nm.Entities {
		oe := oldByName[ne.Name]
		declared := false
		if oe == nil {
			if from := next.Entities[i].RenamedFrom; from != "" && !newNames[from] {
				oe, declared = oldByName[from], true
			}
		}
		if oe == nil {
			creates = append(creates, MigrationChange{
				Kind: ChangeCreateTable, Table: ne.TableName,
				Detail: fmt.Sprintf("create table %s for the new entity %s", ne.Table, ne.Name),
				SQL:    []string{createTableSQL(ne)},
			})
			continue
		}
		matched[oe.Name] = true
		d := tableDiff{table: ne.TableName, ident: ne.Table, ne: ne}
		if oe.TableName != ne.TableName {
			why := "the plural changed"
			if declared {
				why = "renamed_from: " + oe.Name
			}
			c := MigrationChange{
				Kind: ChangeRenameTable, Table: ne.TableName,
				Detail: fmt.Sprintf("rename table %s to %s (%s)", oe.Table, ne.Table, why),
				SQL:    []string{fmt.Sprintf("ALTER TABLE %s RENAME TO %s;", oe.Table, ne.Table)},
			}
			// Unique index names carry the table name and are what the store
			// reports on conflicts, so they follow the table.
			for _, of := range oe.Fields {
				if nf := matchField(ne, of.Name); of.Unique && nf != nil && nf.Name == of.Name && nf.Unique {
					c.SQL = append(c.SQL, renameIndexSQL(oe, of, ne, nf))
				}
			}
			renames = append(renames, c)
		}
		d.diff(oe, ne)
		tables = append(tables, d.changes()...)
	}
	for _, oe := range pm.Entities {
		if matched[oe.Name] {
			continue
		}
		detail := fmt.Sprintf("drop table %s: entity %s was removed and its rows are lost", oe.Table, oe.Name)
		if len(creates) > 0 {
			detail += fmt.Sprintf("; if it was renamed, set renamed_from: %s on the new entity", oe.Name)
		}
		drops = append(drops, MigrationChange{
			Kind: ChangeDropTable, Table: oe.TableName, Destructive: true, Detail: detail,
			SQL: []string{fmt.Sprintf("DROP TABLE %s;", oe.Table)},
		})
	}
	// Drops first and renames before creates, so a name freed in this
	// migration can be taken again in it.
	plan.Changes = slices.Concat(drops, renames, creates, tables)
	return plan
}

// matchField returns the field of ne that corresponds to the previous
// version's field old: the same name, or a field renamed from it.
func matchField(ne *EntityModel, old string) *FieldModel {
	for _, f := range ne.Fields {
		if f.Name == old {
			return f
		}
	}
	for _, f := range ne.Fields {
		if f.RenamedFrom == old && !slices.ContainsFunc(ne.Fields, func(g *FieldModel) bool { return g.Name == old }) {
			return f
		}
	}
	return nil
}

// tableDiff collects the column changes of one table by phase, so that the
// SQL runs in a safe order: drops, renames, alterations, additions.
type tableDiff struct {
	table, ident                     string
	ne                               *EntityModel
	drops, renames, alters, addition []MigrationChange
}

func (d *tableDiff) changes() []MigrationChange {
	return slices.Concat(d.drops, d.renames, d.alters, d.addition)
}

func (d *tableDiff) diff(oe, ne *EntityModel) {
	pairs := map[string]*FieldModel{} // new field name -> old field
	var added []*FieldModel
	for _, of := range oe.Fields {
		if nf := matchField(ne, of.Name); nf != nil {
			pairs[nf.Name] = of
		}
	}
	for _, nf := range ne.Fields {
		if pairs[nf.Name] == nil {
			added = append(added, nf)
		}
	}
	for _, of := range oe.Fields {
		if matchField(ne, of.Name) != nil {
			continue
		}
		detail := fmt.Sprintf("drop column %s.%s: its data is lost", d.ident, of.Col.Ident)
		for _, nf := range added {
			if nf.Col.Type == of.Col.Type {
				detail += fmt.Sprintf("; if it was renamed to %s, set renamed_from: %s on that field", nf.Name, of.Name)
				break
			}
		}
		d.drops = append(d.drops, MigrationChange{
			Kind: ChangeDropColumn, Table: d.table, Column: of.Name, Destructive: true, Detail: detail,
			SQL: []string{fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s;", d.ident, of.Col.Ident)},
		})
	}
	for _, nf := range ne.Fields {
		of := pairs[nf.Name]
		if of == nil {
			d.add(nf)
			continue
		}
		if of.Name != nf.Name {
			c := MigrationChange{
				Kind: ChangeRenameColumn, Table: d.table, Column: nf.Name,
				Detail: fmt.Sprintf("rename column %s.%s to %s (renamed_from)", d.ident, of.Col.Ident, nf.Col.Ident),
				SQL:    []string{fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s;", d.ident, of.Col.Ident, nf.Col.Ident)},
			}
			if of.Unique && nf.Unique {
				c.SQL = append(c.SQL, renameIndexSQL(oe, of, ne, nf))
			}
			d.renames = append(d.renames, c)
		}
		d.alter(oe, of, ne, nf)
	}
}

func uniqueIndexName(e *EntityModel, f *FieldModel) string {
	return e.PluralSnake + "_" + f.Name + "_key"
}

func renameIndexSQL(oe *EntityModel, of *FieldModel, ne *EntityModel, nf *FieldModel) string {
	return fmt.Sprintf("ALTER INDEX %s RENAME TO %s;", quoteIdent(uniqueIndexName(oe, of)), quoteIdent(uniqueIndexName(ne, nf)))
}

func createUniqueSQL(table string, e *EntityModel, f *FieldModel) string {
	return fmt.Sprintf("CREATE UNIQUE INDEX %s ON %s (%s) WHERE %s <> '';", quoteIdent(uniqueIndexName(e, f)), table, f.Col.Ident, f.Col.Ident)
}

// add adds a column for a new field.
func (d *tableDiff) add(nf *FieldModel) {
	c := MigrationChange{Kind: ChangeAddColumn, Table: d.table, Column: nf.Name}
	if zeroViolates(nf) {
		c.Destructive = true
		c.Detail = fmt.Sprintf("add required column %s.%s: existing rows get the zero value, which its constraints reject; "+
			"add it as optional first, fill it in, then make it required", d.ident, nf.Col.Ident)
		c.SQL = append(c.SQL, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s;", d.ident, columnDef(nf.Col)))
		c.SQL = append(c.SQL, addChecksSQL(d.ident, nf.Col.Checks, always)...)
	} else {
		c.Detail = fmt.Sprintf("add column %s.%s %s", d.ident, nf.Col.Ident, nf.Col.Type)
		if nf.Col.Default != "" {
			c.Detail += " DEFAULT " + nf.Col.Default
		}
		c.SQL = append(c.SQL, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s;", d.ident, nf.Col.DDL))
	}
	if nf.Unique {
		c.SQL = append(c.SQL, createUniqueSQL(d.ident, d.ne, nf))
	}
	d.addition = append(d.addition, c)
}

// alter compares a field with its previous version.
func (d *tableDiff) alter(oe *EntityModel, of *FieldModel, ne *EntityModel, nf *FieldModel) {
	oc, nc := of.Col, nf.Col
	if of.Unique && !nf.Unique {
		d.alters = append(d.alters, MigrationChange{
			Kind: ChangeDropUnique, Table: d.table, Column: nf.Name,
			Detail: fmt.Sprintf("drop the unique index on %s.%s", d.ident, nc.Ident),
			// Renames carry an index along only when the field stays unique,
			// so this one still has its old name.
			SQL: []string{fmt.Sprintf("DROP INDEX %s;", quoteIdent(uniqueIndexName(oe, of)))},
		})
	}
	// PostgreSQL rewrites constraints when a column is renamed, so the old
	// checks are compared under the new name.
	oldChecks := make([]string, len(oc.Checks))
	for i, ch := range oc.Checks {
		oldChecks[i] = strings.ReplaceAll(ch, oc.Ident, nc.Ident)
	}
	checksChanged := !slices.Equal(oldChecks, nc.Checks)
	tighter := checksChanged && checksTighter(of, nf, oldChecks)
	switch {
	case oc.Type != nc.Type:
		widen := safeWidening(oc.Type, nc.Type)
		c := MigrationChange{Kind: ChangeAlterType, Table: d.table, Column: nf.Name, Destructive: !widen || tighter}
		switch {
		case !widen:
			c.Detail = fmt.Sprintf("change %s.%s from %s to %s: values may not convert", d.ident, nc.Ident, oc.Type, nc.Type)
		case tighter:
			c.Detail = fmt.Sprintf("widen %s.%s from %s to %s with tighter constraints that existing rows may violate", d.ident, nc.Ident, oc.Type, nc.Type)
		default:
			c.Detail = fmt.Sprintf("widen %s.%s from %s to %s; this rewrites the table under a lock", d.ident, nc.Ident, oc.Type, nc.Type)
		}
		if checksChanged {
			c.SQL = append(c.SQL, dropChecksSQL(d.table, nf.Name))
		}
		if oc.Default != "" {
			c.SQL = append(c.SQL, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s DROP DEFAULT;", d.ident, nc.Ident))
		}
		c.SQL = append(c.SQL, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s TYPE %s USING %s;", d.ident, nc.Ident, nc.Type, usingExpr(nc.Ident, oc, nc)))
		if nc.Default != "" {
			c.SQL = append(c.SQL, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s;", d.ident, nc.Ident, nc.Default))
		}
		switch {
		case nc.NotNull && !oc.NotNull:
			c.SQL = append(c.SQL, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL;", d.ident, nc.Ident))
		case !nc.NotNull && oc.NotNull:
			c.SQL = append(c.SQL, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s DROP NOT NULL;", d.ident, nc.Ident))
		}
		if checksChanged {
			c.SQL = append(c.SQL, addChecksSQL(d.ident, nc.Checks, mayFail(tighter, oldChecks))...)
		}
		d.alters = append(d.alters, c)
	case checksChanged:
		c := MigrationChange{Kind: ChangeAlterCheck, Table: d.table, Column: nf.Name, Destructive: tighter}
		if tighter {
			c.Detail = fmt.Sprintf("tighten the constraints on %s.%s: existing rows may violate them", d.ident, nc.Ident)
		} else {
			c.Detail = fmt.Sprintf("loosen the constraints on %s.%s", d.ident, nc.Ident)
		}
		c.SQL = append(c.SQL, dropChecksSQL(d.table, nf.Name))
		c.SQL = append(c.SQL, addChecksSQL(d.ident, nc.Checks, mayFail(tighter, oldChecks))...)
		d.alters = append(d.alters, c)
	}
	if !of.Unique && nf.Unique {
		d.alters = append(d.alters, MigrationChange{
			Kind: ChangeAddUnique, Table: d.table, Column: nf.Name, Destructive: true,
			Detail: fmt.Sprintf("make %s.%s unique: fails if existing rows share a value", d.ident, nc.Ident),
			SQL:    []string{createUniqueSQL(d.ident, ne, nf)},
		})
	}
}

// columnDef is a column definition without its CHECK constraints.
func columnDef(c *ColumnModel) string {
	s := c.Ident + " " + c.Type
	if c.NotNull {
		s += " NOT NULL"
	}
	if c.Default != "" {
		s += " DEFAULT " + c.Default
	}
	return s
}

// addChecksSQL adds CHECK constraints. Those that existing rows may
// violate are added NOT VALID: PostgreSQL enforces them for new and updated
// rows and leaves existing ones to a later VALIDATE CONSTRAINT.
func addChecksSQL(table string, checks []string, mayFail func(check string) bool) []string {
	var out []string
	for _, ch := range checks {
		s := fmt.Sprintf("ALTER TABLE %s ADD CHECK (%s)", table, ch)
		if mayFail(ch) {
			s += " NOT VALID"
		}
		out = append(out, s+";")
	}
	return out
}

// mayFail tells which new checks existing rows may violate: none when the
// constraints did not get tighter, else those that were not there before.
func mayFail(tighter bool, old []string) func(string) bool {
	return func(ch string) bool { return tighter && !slices.Contains(old, ch) }
}

func always(string) bool { return true }

// dropChecksSQL drops every single-column CHECK constraint of a column.
// The first migration declares them inline, so PostgreSQL chose their
// names; they are found in the catalog instead of guessed. Table and column
// names are lower_snake_case, so they need no escaping in the literals.
func dropChecksSQL(table, column string) string {
	return fmt.Sprintf(`DO $$
DECLARE c text;
BEGIN
  FOR c IN SELECT con.conname FROM pg_constraint con
      JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = con.conkey[1]
      WHERE con.conrelid = '%s'::regclass AND con.contype = 'c'
        AND cardinality(con.conkey) = 1 AND a.attname = '%s'
  LOOP
    EXECUTE format('ALTER TABLE %%I DROP CONSTRAINT %%I', '%s', c);
  END LOOP;
END $$;`, quoteIdent(table), column, table)
}

// safeWidening reports whether every value of SQL type from fits type to.
func safeWidening(from, to string) bool {
	fa, ta := strings.HasSuffix(from, "[]"), strings.HasSuffix(to, "[]")
	if fa != ta {
		return false
	}
	from, to = strings.TrimSuffix(from, "[]"), strings.TrimSuffix(to, "[]")
	wider := map[string][]string{
		"integer": {"bigint", "numeric(20,0)", "double precision"},
		"bigint":  {"numeric(20,0)"},
		"real":    {"double precision"},
	}
	return slices.Contains(wider[from], to)
}

// usingExpr converts the old column value for ALTER COLUMN ... TYPE.
func usingExpr(col string, oc, nc *ColumnModel) string {
	oa, na := strings.HasSuffix(oc.Type, "[]"), strings.HasSuffix(nc.Type, "[]")
	elem := strings.TrimSuffix(nc.Type, "[]")
	var e string
	switch {
	case !oa && na:
		e = fmt.Sprintf("CASE WHEN %s IS NULL THEN '{}' ELSE ARRAY[%s::%s] END", col, col, elem)
	case oa && !na:
		e = fmt.Sprintf("(%s)[1]::%s", col, elem)
	default:
		e = col + "::" + nc.Type
	}
	if nc.NotNull && nc.Default != "" && !na && (!oc.NotNull || oa) {
		e = fmt.Sprintf("COALESCE(%s, %s)", e, nc.Default)
	}
	return e
}

// zeroViolates reports whether a column's constraints reject the value
// existing rows get when it is added.
func zeroViolates(f *FieldModel) bool {
	switch {
	case f.Repeated, f.Type == "timestamp", f.Type == "string":
		return f.Required
	case spec.IsNumeric(f.Type):
		lo, hi := bounds(f)
		return lo > 0 || hi < 0
	}
	return false
}

// bounds returns the numeric range a field's constraints allow.
func bounds(f *FieldModel) (lo, hi float64) {
	lo, hi = math.Inf(-1), math.Inf(1)
	if f.Min != nil {
		lo = *f.Min
	}
	if f.Max != nil {
		hi = *f.Max
	}
	if strings.HasPrefix(f.Type, "uint") {
		lo = max(lo, 0)
	}
	return lo, hi
}

func lengths(f *FieldModel) (lo, hi int) {
	lo, hi = 0, math.MaxInt
	if f.MinLen != nil {
		lo = *f.MinLen
	}
	if f.MaxLen != nil {
		hi = *f.MaxLen
	}
	return lo, hi
}

// checksTighter reports whether the new constraints of a column may reject
// a row that the old ones accepted. With the same type the spec's bounds
// are compared; across types, any CHECK that was not there before counts.
func checksTighter(of, nf *FieldModel, oldChecks []string) bool {
	if of.Type != nf.Type || of.Repeated != nf.Repeated {
		for _, ch := range nf.Col.Checks {
			if !slices.Contains(oldChecks, ch) {
				return true
			}
		}
		return false
	}
	if nf.Required && !of.Required {
		return true
	}
	olo, ohi := bounds(of)
	nlo, nhi := bounds(nf)
	if nlo > olo || nhi < ohi {
		return true
	}
	ollo, olhi := lengths(of)
	nllo, nlhi := lengths(nf)
	return nllo > ollo || nlhi < olhi
}

// createTableSQL is the CREATE TABLE of a new entity, in the format of the
// first migration.
func createTableSQL(e *EntityModel) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s (\n  \"id\" text COLLATE \"C\" PRIMARY KEY,\n", e.Table)
	for _, f := range e.Fields {
		fmt.Fprintf(&b, "  %s,\n", f.Col.DDL)
	}
	b.WriteString("  \"create_time\" timestamptz NOT NULL,\n  \"update_time\" timestamptz NOT NULL\n);")
	for _, u := range e.UniqueIndexes {
		fmt.Fprintf(&b, "\n\nCREATE UNIQUE INDEX %s ON %s (%s) WHERE %s <> '';", quoteIdent(u.Name), e.Table, u.Column, u.Column)
	}
	return b.String()
}

// NextMigrationName numbers the migration after the existing files (base
// names such as 0001_init.sql) and names it after the plan's first changes.
func NextMigrationName(existing []string, plan *MigrationPlan) string {
	n := 0
	for _, name := range existing {
		i := 0
		for i < len(name) && name[i] >= '0' && name[i] <= '9' {
			i++
		}
		if v, err := strconv.Atoi(name[:i]); err == nil {
			n = max(n, v)
		}
	}
	var slugs []string
	for _, c := range plan.Changes {
		s := changeSlug(c)
		if !slices.Contains(slugs, s) {
			slugs = append(slugs, s)
		}
	}
	desc := "schema"
	switch {
	case len(slugs) > 2:
		desc = strings.Join(slugs[:2], "_") + "_and_more"
	case len(slugs) > 0:
		desc = strings.Join(slugs, "_")
	}
	if len(desc) > 60 {
		desc = strings.TrimRight(desc[:60], "_")
	}
	return fmt.Sprintf("%04d_%s.sql", n+1, desc)
}

func changeSlug(c MigrationChange) string {
	verb := map[string]string{
		ChangeCreateTable: "add", ChangeDropTable: "drop", ChangeRenameTable: "rename",
		ChangeAddColumn: "add", ChangeDropColumn: "drop", ChangeRenameColumn: "rename",
		ChangeAlterType: "alter", ChangeAlterCheck: "alter", ChangeAddUnique: "unique", ChangeDropUnique: "alter",
	}[c.Kind]
	if c.Column == "" {
		return verb + "_" + c.Table
	}
	return verb + "_" + c.Table + "_" + c.Column
}

// RenderMigration writes the plan as the next migration file of a service
// for spec s. from names where the previous schema came from, for the
// header comment.
func RenderMigration(s *spec.Spec, plan *MigrationPlan, existing []string, from string) File {
	name := NextMigrationName(existing, plan)
	m := NewModel(s)
	var b bytes.Buffer
	fmt.Fprintf(&b, "-- Migration %s for %s, generated by fliessband.\n", strings.TrimSuffix(name, ".sql"), m.FullServiceName)
	fmt.Fprintf(&b, "-- Moves the schema of %s to this version of the spec. Applied by\n", from)
	b.WriteString("-- store.OpenPostgres after the earlier files; never edit it once it has run.\n--\n")
	for _, c := range plan.Changes {
		mark := "safe"
		if c.Destructive {
			mark = "destructive, allowed by migrations.allow_destructive"
		}
		fmt.Fprintf(&b, "--   %s (%s)\n", c.Detail, mark)
	}
	for _, c := range plan.Changes {
		fmt.Fprintf(&b, "\n-- %s\n%s\n", c.Detail, strings.Join(c.SQL, "\n"))
	}
	return File{Path: MigrationsDir + "/" + name, Template: "migration diff", Data: b.Bytes()}
}

// Remove drops the entry for path.
func (m *Manifest) Remove(path string) {
	m.Files = slices.DeleteFunc(m.Files, func(e ManifestEntry) bool { return e.Path == path })
}
