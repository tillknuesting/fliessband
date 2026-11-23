package gen

import (
	"bytes"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/tillknuesting/fliessband/internal/spec"
)

// TestExamplesRender renders every example spec and checks that each Go file
// parses and that no template leaves an unused import behind.
func TestExamplesRender(t *testing.T) {
	paths, _ := filepath.Glob("../../specs/*.yaml")
	if len(paths) == 0 {
		t.Fatal("no example specs")
	}
	paths = append(paths, "testdata/kitchen.yaml")
	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			src, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			s, ps, err := spec.Parse(src)
			if err != nil || spec.HasErrors(ps) {
				t.Fatalf("spec: %v %v", err, ps)
			}
			files, err := Generate(s)
			if err != nil {
				t.Fatal(err)
			}
			again, err := Generate(s)
			if err != nil || len(again) != len(files) {
				t.Fatalf("second render: %v", err)
			}
			for i, f := range files {
				if !bytes.Equal(f.Data, again[i].Data) {
					t.Errorf("%s: rendering is not deterministic", f.Path)
				}
				switch {
				case strings.HasSuffix(f.Path, ".go"):
					if _, err := parser.ParseFile(token.NewFileSet(), f.Path, f.Data, 0); err != nil {
						t.Errorf("%s: %v", f.Path, err)
					}
				case strings.HasSuffix(f.Path, ".yaml"), strings.HasSuffix(f.Path, ".yml"):
					var doc map[string]any
					if err := yaml.Unmarshal(f.Data, &doc); err != nil || len(doc) == 0 {
						t.Errorf("%s: not a YAML document: %v", f.Path, err)
					}
				}
				if bytes.Contains(f.Data, []byte("<no value>")) {
					t.Errorf("%s: a template field rendered as <no value>", f.Path)
				}
			}
		})
	}
}

// TestPostgresSchema checks the DDL derived from the spec rules.
func TestPostgresSchema(t *testing.T) {
	src, err := os.ReadFile("testdata/kitchen.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, _, err := spec.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	files, err := Generate(s)
	if err != nil {
		t.Fatal(err)
	}
	var sql string
	for _, f := range files {
		if strings.HasSuffix(f.Path, ".sql") {
			sql = string(f.Data)
		}
	}
	for _, want := range []string{
		`"id" text COLLATE "C" PRIMARY KEY`,
		`"name" text NOT NULL DEFAULT '' CHECK ("name" <> '')`,
		`CHECK ("label" = '' OR char_length("label") BETWEEN 2 AND 20)`,
		`"small" integer NOT NULL DEFAULT 0 CHECK ("small" BETWEEN -5 AND 5)`,
		`"count" bigint NOT NULL DEFAULT 0 CHECK ("count" BETWEEN 0 AND 4294967295) CHECK ("count" <= 1000)`,
		`"huge" numeric(20,0) NOT NULL DEFAULT 0 CHECK ("huge" >= 0)`,
		`"ratio" real NOT NULL DEFAULT 0 CHECK ("ratio" >= 0.5)`,
		`"due" timestamptz CHECK ("due" IS NOT NULL)`,
		`"times" timestamptz[] NOT NULL DEFAULT '{}' CHECK (cardinality("times") > 0)`,
		`CREATE UNIQUE INDEX "gadgets_name_key" ON "gadgets" ("name") WHERE "name" <> '';`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("schema lacks %s\n%s", want, sql)
		}
	}
}

func TestNames(t *testing.T) {
	cases := map[string][2]string{ // input -> snake, goCamelCase(snake)
		"OrderLine": {"order_line", "OrderLine"},
		"HTTPRoute": {"http_route", "HttpRoute"},
		"Item2Box":  {"item2_box", "Item2Box"},
	}
	for in, want := range cases {
		if got := snake(in); got != want[0] {
			t.Errorf("snake(%q) = %q, want %q", in, got, want[0])
		}
		if got := goCamelCase(want[0]); got != want[1] {
			t.Errorf("goCamelCase(%q) = %q, want %q", want[0], got, want[1])
		}
	}
	if v := safeVar("Type"); v != "typeItem" {
		t.Errorf("safeVar(Type) = %q", v)
	}
}
