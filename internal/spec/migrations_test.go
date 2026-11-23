package spec

import (
	"strings"
	"testing"
)

func TestRenamedFrom(t *testing.T) {
	tests := []struct {
		name, yaml, wantPath string // wantPath "" means valid
	}{
		{"field rename", "name: svc\nentities: [{name: A, fields: [{name: y, type: string, renamed_from: x}]}]", ""},
		{"entity rename", "name: svc\nentities: [{name: B, renamed_from: A, fields: [{name: x, type: string}]}]", ""},
		{"allow destructive", "name: svc\nmigrations: {allow_destructive: true}\nentities: [{name: A, fields: [{name: x, type: string}]}]", ""},
		{"from a current field", "name: svc\nentities: [{name: A, fields: [{name: x, type: string}, {name: y, type: string, renamed_from: x}]}]", "entities[0].fields[1].renamed_from"},
		{"two fields from one", "name: svc\nentities: [{name: A, fields: [{name: y, type: string, renamed_from: x}, {name: z, type: string, renamed_from: x}]}]", "entities[0].fields[1].renamed_from"},
		{"bad field name", "name: svc\nentities: [{name: A, fields: [{name: y, type: string, renamed_from: X}]}]", "entities[0].fields[0].renamed_from"},
		{"from a current entity", "name: svc\nentities: [{name: A, fields: [{name: x, type: string}]}, {name: B, renamed_from: A, fields: [{name: x, type: string}]}]", "entities[1].renamed_from"},
		{"bad entity name", "name: svc\nentities: [{name: B, renamed_from: a_b, fields: [{name: x, type: string}]}]", "entities[0].renamed_from"},
		{"method field", "name: svc\nmethods: [{name: Do, doc: x, request: [{name: a, type: string, renamed_from: b}]}]", "methods[0].request[0].renamed_from"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, ps, err := Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, p := range ps {
				if !p.Warning {
					got = append(got, p.Path)
				}
			}
			switch {
			case tt.wantPath == "" && len(got) > 0:
				t.Errorf("unexpected errors: %v", ps)
			case tt.wantPath != "" && !strings.Contains(strings.Join(got, " "), tt.wantPath):
				t.Errorf("no error at %s: %v", tt.wantPath, ps)
			}
			if tt.name == "allow destructive" && !s.Migrations.AllowDestructive {
				t.Error("allow_destructive not parsed")
			}
		})
	}
}
