package spec

import (
	"strings"
	"testing"
)

func TestParseRejectsBadSpecs(t *testing.T) {
	tests := []struct {
		name, yaml, wantPath string
	}{
		{"bad service name", "name: Bad_Name\nentities: [{name: A, fields: [{name: x, type: string}]}]", "name"},
		{"pattern needs example", "name: s\nentities: [{name: A, fields: [{name: x, type: string, pattern: '^a$'}]}]", "entities[0].fields[0].example"},
		{"unique variant breaks pattern", "name: s\nentities: [{name: A, fields: [{name: x, type: string, unique: true, pattern: '^A1$', example: A1}]}]", "entities[0].fields[0].example"},
		{"required number", "name: s\nentities: [{name: A, fields: [{name: n, type: int32, required: true}]}]", "entities[0].fields[0].required"},
		{"bound outside int32", "name: s\nentities: [{name: A, fields: [{name: n, type: int32, max: 3000000000}]}]", "entities[0].fields[0].max"},
		{"reserved field", "name: s\nentities: [{name: A, fields: [{name: id, type: string}]}]", "entities[0].fields[0].name"},
		{"rpc clash", "name: s\nentities: [{name: A, fields: [{name: x, type: string}]}]\nmethods: [{name: CreateA, doc: x}]", "methods[0]"},
		{"unknown type in method", "name: s\nmethods: [{name: Do, doc: x, request: [{name: a, type: Nope}]}]", "methods[0].request[0].type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ps, err := Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range ps {
				if !p.Warning && strings.HasPrefix(p.Path, tt.wantPath) {
					return
				}
			}
			t.Fatalf("no error at %s; got %v", tt.wantPath, ps)
		})
	}
}

func TestUnknownKeysAreErrors(t *testing.T) {
	if _, _, err := Parse([]byte("name: s\nentitys: []\n")); err == nil {
		t.Fatal("misspelled key accepted")
	}
}

func TestVariantAndPlural(t *testing.T) {
	for in, want := range map[[2]any]string{{"SKU-001", 4}: "SKU-005", {"room", 2}: "room2", {"A09", 1}: "A10"} {
		if got := Variant(in[0].(string), in[1].(int)); got != want {
			t.Errorf("Variant(%v) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"Category": "Categories", "Box": "Boxes", "Key": "Keys", "Task": "Tasks"} {
		if got := Pluralize(in); got != want {
			t.Errorf("Pluralize(%q) = %q, want %q", in, got, want)
		}
	}
}
