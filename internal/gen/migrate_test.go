package gen

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/tillknuesting/fliessband/internal/spec"
)

const baseSpec = `name: shop
package: acme.shop.v1
description: a shop
entities:
  - name: Product
    fields:
      - {name: sku, type: string, required: true, unique: true, pattern: "^SKU-[0-9]{3,6}$", example: "SKU-001"}
      - {name: title, type: string, max_len: 100}
      - {name: stock, type: int32, min: 0}
      - {name: rating, type: float}
      - {name: tags, type: string, repeated: true}
      - {name: released, type: timestamp}
      - {name: legacy_code, type: string}
  - name: Supplier
    fields:
      - {name: name, type: string, required: true}
`

func mustSpec(t *testing.T, src string) *spec.Spec {
	t.Helper()
	s, ps, err := spec.Parse([]byte(src))
	if err != nil || spec.HasErrors(ps) {
		t.Fatalf("spec: %v %v", err, ps)
	}
	return s
}

// edit replaces exactly one occurrence of old in the base spec.
func edit(t *testing.T, src, old, repl string) string {
	t.Helper()
	if strings.Count(src, old) != 1 {
		t.Fatalf("%q does not occur exactly once", old)
	}
	return strings.Replace(src, old, repl, 1)
}

type wantChange struct {
	kind, table, column string
	destructive         bool
}

func TestPlanMigration(t *testing.T) {
	stock := "      - {name: stock, type: int32, min: 0}\n"
	title := "      - {name: title, type: string, max_len: 100}\n"
	sku := `      - {name: sku, type: string, required: true, unique: true, pattern: "^SKU-[0-9]{3,6}$", example: "SKU-001"}` + "\n"
	supplier := "  - name: Supplier\n    fields:\n      - {name: name, type: string, required: true}\n"
	tests := []struct {
		name    string
		next    func(t *testing.T) string
		want    []wantChange
		sql     []string // fragments the SQL must contain
		notSQL  []string
		hintFor string // a fragment the first change's detail must contain
	}{
		{name: "unchanged", next: func(*testing.T) string { return baseSpec }},
		{
			name: "docs and patterns do not touch the schema",
			next: func(t *testing.T) string {
				return edit(t, baseSpec, "a shop", "a better shop")
			},
		},
		{
			name: "new entity",
			next: func(*testing.T) string {
				return baseSpec + "  - name: Review\n    fields:\n      - {name: body, type: string, unique: true, required: true}\n"
			},
			want: []wantChange{{ChangeCreateTable, "reviews", "", false}},
			sql:  []string{`CREATE TABLE "reviews" (`, `"body" text NOT NULL DEFAULT '' CHECK ("body" <> '')`, `CREATE UNIQUE INDEX "reviews_body_key" ON "reviews" ("body") WHERE "body" <> '';`},
		},
		{
			name: "optional column",
			next: func(t *testing.T) string {
				return edit(t, baseSpec, stock, stock+"      - {name: color, type: string, max_len: 20}\n      - {name: weight, type: double}\n      - {name: seen, type: timestamp}\n")
			},
			want: []wantChange{{ChangeAddColumn, "products", "color", false}, {ChangeAddColumn, "products", "weight", false}, {ChangeAddColumn, "products", "seen", false}},
			sql: []string{
				`ALTER TABLE "products" ADD COLUMN "color" text NOT NULL DEFAULT '' CHECK ("color" = '' OR char_length("color") <= 20);`,
				`ALTER TABLE "products" ADD COLUMN "weight" double precision NOT NULL DEFAULT 0;`,
				`ALTER TABLE "products" ADD COLUMN "seen" timestamptz;`,
			},
		},
		{
			name: "new unique column needs no backfill",
			next: func(t *testing.T) string {
				return edit(t, baseSpec, stock, stock+"      - {name: barcode, type: string, unique: true}\n")
			},
			want: []wantChange{{ChangeAddColumn, "products", "barcode", false}},
			sql:  []string{`CREATE UNIQUE INDEX "products_barcode_key" ON "products" ("barcode") WHERE "barcode" <> '';`},
		},
		{
			name: "required column",
			next: func(t *testing.T) string {
				return edit(t, baseSpec, stock, stock+"      - {name: brand, type: string, required: true}\n")
			},
			want:    []wantChange{{ChangeAddColumn, "products", "brand", true}},
			sql:     []string{`ADD COLUMN "brand" text NOT NULL DEFAULT '';`, `ALTER TABLE "products" ADD CHECK ("brand" <> '') NOT VALID;`},
			hintFor: "add it as optional first",
		},
		{
			name: "column whose zero value is out of range",
			next: func(t *testing.T) string {
				return edit(t, baseSpec, stock, stock+"      - {name: pack, type: int32, min: 1}\n")
			},
			want: []wantChange{{ChangeAddColumn, "products", "pack", true}},
		},
		{
			name: "dropped column with a rename hint",
			next: func(t *testing.T) string {
				return edit(t, baseSpec, "      - {name: legacy_code, type: string}\n", "      - {name: code, type: string}\n")
			},
			want:    []wantChange{{ChangeDropColumn, "products", "legacy_code", true}, {ChangeAddColumn, "products", "code", false}},
			sql:     []string{`ALTER TABLE "products" DROP COLUMN "legacy_code";`},
			hintFor: "set renamed_from: legacy_code",
		},
		{
			name: "declared column rename",
			next: func(t *testing.T) string {
				s := edit(t, baseSpec, "      - {name: legacy_code, type: string}\n", "      - {name: code, type: string, renamed_from: legacy_code}\n")
				return edit(t, s, `{name: sku, type: string,`, `{name: item_sku, renamed_from: sku, type: string,`)
			},
			want: []wantChange{{ChangeRenameColumn, "products", "item_sku", false}, {ChangeRenameColumn, "products", "code", false}},
			sql: []string{
				`ALTER TABLE "products" RENAME COLUMN "legacy_code" TO "code";`,
				`ALTER TABLE "products" RENAME COLUMN "sku" TO "item_sku";`,
				`ALTER INDEX "products_sku_key" RENAME TO "products_item_sku_key";`,
			},
		},
		{
			name: "dropped entity",
			next: func(t *testing.T) string { return edit(t, baseSpec, supplier, "") },
			want: []wantChange{{ChangeDropTable, "suppliers", "", true}},
			sql:  []string{`DROP TABLE "suppliers";`},
		},
		{
			name: "declared entity rename",
			next: func(t *testing.T) string {
				return edit(t, baseSpec, "  - name: Product\n", "  - name: Item\n    renamed_from: Product\n")
			},
			want: []wantChange{{ChangeRenameTable, "items", "", false}},
			sql:  []string{`ALTER TABLE "products" RENAME TO "items";`, `ALTER INDEX "products_sku_key" RENAME TO "items_sku_key";`},
		},
		{
			name:    "entity renamed without renamed_from",
			next:    func(t *testing.T) string { return edit(t, baseSpec, "  - name: Supplier\n", "  - name: Vendor\n") },
			want:    []wantChange{{ChangeDropTable, "suppliers", "", true}, {ChangeCreateTable, "vendors", "", false}},
			hintFor: "set renamed_from: Supplier",
		},
		{
			name: "changed plural renames the table",
			next: func(t *testing.T) string {
				return edit(t, baseSpec, "  - name: Supplier\n", "  - name: Supplier\n    plural: Vendors\n")
			},
			want: []wantChange{{ChangeRenameTable, "vendors", "", false}},
			sql:  []string{`ALTER TABLE "suppliers" RENAME TO "vendors";`},
		},
		{
			name: "widened integer",
			next: func(t *testing.T) string {
				return edit(t, baseSpec, "{name: stock, type: int32,", "{name: stock, type: int64,")
			},
			want: []wantChange{{ChangeAlterType, "products", "stock", false}},
			sql:  []string{`ALTER TABLE "products" ALTER COLUMN "stock" TYPE bigint USING "stock"::bigint;`, `SET DEFAULT 0;`},
			// The CHECK is the same expression, so it stays.
			notSQL: []string{"pg_constraint"},
		},
		{
			name: "widened float",
			next: func(t *testing.T) string {
				return edit(t, baseSpec, "{name: rating, type: float}", "{name: rating, type: double}")
			},
			want: []wantChange{{ChangeAlterType, "products", "rating", false}},
		},
		{
			name: "unrelated type change",
			next: func(t *testing.T) string {
				return edit(t, baseSpec, "{name: legacy_code, type: string}", "{name: legacy_code, type: int64}")
			},
			want: []wantChange{{ChangeAlterType, "products", "legacy_code", true}},
			sql:  []string{`TYPE bigint USING "legacy_code"::bigint;`, `ALTER COLUMN "legacy_code" SET DEFAULT 0;`},
		},
		{
			name: "scalar to repeated",
			next: func(t *testing.T) string {
				return edit(t, baseSpec, "{name: legacy_code, type: string}", "{name: legacy_code, type: string, repeated: true}")
			},
			want: []wantChange{{ChangeAlterType, "products", "legacy_code", true}},
			sql:  []string{`TYPE text[] USING CASE WHEN "legacy_code" IS NULL THEN '{}' ELSE ARRAY["legacy_code"::text] END;`},
		},
		{
			name: "looser length",
			next: func(t *testing.T) string {
				return edit(t, baseSpec, title, "      - {name: title, type: string, max_len: 200}\n")
			},
			want:   []wantChange{{ChangeAlterCheck, "products", "title", false}},
			sql:    []string{`a.attname = 'title'`, `'"products"'::regclass`, `ALTER TABLE "products" ADD CHECK ("title" = '' OR char_length("title") <= 200);`},
			notSQL: []string{"NOT VALID"},
		},
		{
			name: "dropped constraint",
			next: func(t *testing.T) string { return edit(t, baseSpec, stock, "      - {name: stock, type: int32}\n") },
			want: []wantChange{{ChangeAlterCheck, "products", "stock", false}},
		},
		{
			name: "tighter length",
			next: func(t *testing.T) string {
				return edit(t, baseSpec, title, "      - {name: title, type: string, max_len: 50}\n")
			},
			want: []wantChange{{ChangeAlterCheck, "products", "title", true}},
			sql:  []string{`ADD CHECK ("title" = '' OR char_length("title") <= 50) NOT VALID;`},
		},
		{
			name: "newly required",
			next: func(t *testing.T) string {
				return edit(t, baseSpec, title, "      - {name: title, type: string, required: true, max_len: 100}\n")
			},
			want: []wantChange{{ChangeAlterCheck, "products", "title", true}},
			sql:  []string{`ADD CHECK ("title" <> '') NOT VALID;`, `ADD CHECK (char_length("title") <= 100) NOT VALID;`},
		},
		{
			name: "required timestamp means NOT NULL without a default",
			next: func(t *testing.T) string {
				return edit(t, baseSpec, "{name: released, type: timestamp}", "{name: released, type: timestamp, required: true}")
			},
			want: []wantChange{{ChangeAlterCheck, "products", "released", true}},
			sql:  []string{`ADD CHECK ("released" IS NOT NULL) NOT VALID;`},
		},
		{
			name: "new unique index",
			next: func(t *testing.T) string {
				return edit(t, baseSpec, title, "      - {name: title, type: string, max_len: 100, unique: true}\n")
			},
			want: []wantChange{{ChangeAddUnique, "products", "title", true}},
			sql:  []string{`CREATE UNIQUE INDEX "products_title_key" ON "products" ("title") WHERE "title" <> '';`},
		},
		{
			name: "dropped unique index",
			next: func(t *testing.T) string {
				return edit(t, baseSpec, sku, `      - {name: sku, type: string, required: true, pattern: "^SKU-[0-9]{3,6}$", example: "SKU-001"}`+"\n")
			},
			want: []wantChange{{ChangeDropUnique, "products", "sku", false}},
			sql:  []string{`DROP INDEX "products_sku_key";`},
		},
		{
			name: "signed to unsigned adds a range check",
			next: func(t *testing.T) string { return edit(t, baseSpec, stock, "      - {name: stock, type: uint32}\n") },
			want: []wantChange{{ChangeAlterType, "products", "stock", true}},
		},
	}
	prev := mustSpec(t, baseSpec)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := PlanMigration(prev, mustSpec(t, tt.next(t)))
			var got []wantChange
			var sql []string
			for _, c := range plan.Changes {
				got = append(got, wantChange{c.Kind, c.Table, c.Column, c.Destructive})
				sql = append(sql, c.SQL...)
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("changes = %v\nwant      %v", got, tt.want)
			}
			all := strings.Join(sql, "\n")
			for _, frag := range tt.sql {
				if !strings.Contains(all, frag) {
					t.Errorf("SQL lacks %q:\n%s", frag, all)
				}
			}
			for _, frag := range tt.notSQL {
				if strings.Contains(all, frag) {
					t.Errorf("SQL contains %q:\n%s", frag, all)
				}
			}
			if tt.hintFor != "" && (len(plan.Changes) == 0 || !strings.Contains(plan.Changes[0].Detail, tt.hintFor)) {
				t.Errorf("first change does not say %q: %+v", tt.hintFor, plan.Changes)
			}
			wantBlocked := slices.ContainsFunc(tt.want, func(w wantChange) bool { return w.destructive })
			if plan.Blocked() != wantBlocked {
				t.Errorf("blocked = %v, want %v", plan.Blocked(), wantBlocked)
			}
		})
	}
}

func TestPlanMigrationRetypedIntegerRange(t *testing.T) {
	// int64 to int32 keeps the SQL type name family but narrows it.
	prev := mustSpec(t, edit(t, baseSpec, "{name: stock, type: int32,", "{name: stock, type: int64,"))
	plan := PlanMigration(prev, mustSpec(t, baseSpec))
	if len(plan.Changes) != 1 || plan.Changes[0].Kind != ChangeAlterType || !plan.Changes[0].Destructive {
		t.Fatalf("bigint to integer should be a destructive type change: %+v", plan.Changes)
	}
	// uint32 to int64 keeps bigint and drops the range check.
	unsigned := edit(t, baseSpec, "{name: stock, type: int32, min: 0}", "{name: stock, type: uint32}")
	plan = PlanMigration(mustSpec(t, unsigned), mustSpec(t, edit(t, unsigned, "{name: stock, type: uint32}", "{name: stock, type: int64}")))
	if len(plan.Changes) != 1 || plan.Changes[0].Kind != ChangeAlterCheck || plan.Changes[0].Destructive {
		t.Fatalf("uint32 to int64 should loosen a check: %+v", plan.Changes)
	}
	// real to numeric adds a ">= 0" check that negative rows violate.
	plan = PlanMigration(mustSpec(t, baseSpec), mustSpec(t, edit(t, baseSpec, "{name: rating, type: float}", "{name: rating, type: uint64}")))
	if len(plan.Changes) != 1 || !plan.Changes[0].Destructive {
		t.Fatalf("real to numeric is not a widening: %+v", plan.Changes)
	}
}

func TestAllowDestructive(t *testing.T) {
	next := edit(t, baseSpec, "  - name: Supplier\n    fields:\n      - {name: name, type: string, required: true}\n", "") +
		"migrations: {allow_destructive: true}\n"
	plan := PlanMigration(mustSpec(t, baseSpec), mustSpec(t, next))
	if len(plan.Destructive()) != 1 || plan.Blocked() {
		t.Fatalf("destructive %d, blocked %v; want 1 allowed change", len(plan.Destructive()), plan.Blocked())
	}
}

func TestNextMigrationName(t *testing.T) {
	plan := &MigrationPlan{Changes: []MigrationChange{
		{Kind: ChangeCreateTable, Table: "reviews"},
		{Kind: ChangeAddColumn, Table: "products", Column: "color"},
		{Kind: ChangeAddColumn, Table: "products", Column: "size"},
	}}
	if got := NextMigrationName([]string{"0001_init.sql", "0002_add_x.sql"}, plan); got != "0003_add_reviews_add_products_color_and_more.sql" {
		t.Errorf("name = %q", got)
	}
	if got := NextMigrationName([]string{"0001_init.sql"}, &MigrationPlan{Changes: plan.Changes[:1]}); got != "0002_add_reviews.sql" {
		t.Errorf("name = %q", got)
	}
	long := &MigrationPlan{Changes: []MigrationChange{{Kind: ChangeAddColumn, Table: strings.Repeat("t", 40), Column: strings.Repeat("c", 40)}}}
	if got := NextMigrationName(nil, long); len(got) > len("0001_.sql")+60 || !strings.HasPrefix(got, "0001_add_") {
		t.Errorf("name = %q", got)
	}
}

// TestRenderMigrationChain renders version 1 with the generator and the
// migration to version 2, and checks that the chain describes version 2.
func TestRenderMigrationChain(t *testing.T) {
	v1 := mustSpec(t, baseSpec)
	v2src := edit(t, baseSpec, "      - {name: stock, type: int32, min: 0}\n",
		"      - {name: stock, type: int64, min: 0}\n      - {name: color, type: string, max_len: 20}\n") +
		"  - name: Review\n    fields:\n      - {name: body, type: string, required: true}\n"
	v2 := mustSpec(t, v2src)

	files, err := Generate(v1)
	if err != nil {
		t.Fatal(err)
	}
	var init *File
	for i := range files {
		if strings.HasPrefix(files[i].Path, MigrationsDir+"/") {
			if init != nil {
				t.Fatalf("a first version renders more than one migration: %s", files[i].Path)
			}
			init = &files[i]
		}
	}
	if init == nil || init.Path != MigrationsDir+"/0001_init.sql" {
		t.Fatalf("no 0001_init.sql: %+v", init)
	}

	plan := PlanMigration(v1, v2)
	if plan.Blocked() || len(plan.Changes) != 3 {
		t.Fatalf("plan = %+v", plan.Changes)
	}
	f := RenderMigration(v2, plan, []string{"0001_init.sql"}, "run r1")
	if f.Path != MigrationsDir+"/0002_add_reviews_alter_products_stock_and_more.sql" {
		t.Errorf("path = %s", f.Path)
	}
	sql := string(f.Data)
	for _, want := range []string{
		"-- Migration 0002_add_reviews_alter_products_stock_and_more for acme.shop.v1.ShopService",
		"-- Moves the schema of run r1",
		"create table \"reviews\" for the new entity Review (safe)",
		"CREATE TABLE \"reviews\" (\n  \"id\" text COLLATE \"C\" PRIMARY KEY,\n  \"body\" text NOT NULL DEFAULT '' CHECK (\"body\" <> ''),\n",
		`ALTER TABLE "products" ADD COLUMN "color" text NOT NULL DEFAULT '' CHECK ("color" = '' OR char_length("color") <= 20);`,
		`ALTER TABLE "products" ALTER COLUMN "stock" TYPE bigint USING "stock"::bigint;`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("0002 lacks %q:\n%s", want, sql)
		}
	}
	// The new table matches what a first version of v2 would create.
	files2, err := Generate(v2)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range files2 {
		if g.Path == MigrationsDir+"/0001_init.sql" {
			created := createTableSQL(NewModel(v2).Entities[2])
			if !strings.Contains(string(g.Data), created) {
				t.Errorf("CREATE TABLE in the migration differs from the first-version template:\n%s\n---\n%s", created, g.Data)
			}
		}
	}
	// No changes, no plan.
	if p := PlanMigration(v2, v2); len(p.Changes) != 0 {
		t.Errorf("a spec against itself has changes: %+v", p.Changes)
	}
}

func TestManifestRemove(t *testing.T) {
	m := NewManifest([]byte("x"), []File{{Path: "a"}, {Path: "b"}})
	m.Remove("a")
	if m.Origin("a") != "" || m.Origin("b") == "" {
		t.Errorf("files = %+v", m.Files)
	}
}
