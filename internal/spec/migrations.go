package spec

import "fmt"

// Migrations controls the schema migration generated between versions of
// a service. Every run carries the previous version's migration files
// forward and adds one that moves the schema to this spec. Changes that can
// lose data or reject existing rows stop the line unless AllowDestructive
// is set. Renames are declared with renamed_from on the entity or field.
type Migrations struct {
	// AllowDestructive lets drops, narrowing type changes and constraints
	// that existing rows may violate through. Set it for the version that
	// needs it, then remove it again.
	AllowDestructive bool `yaml:"allow_destructive" json:"allow_destructive,omitempty"`
}

// validateRenames checks renamed_from on entities and fields.
func (s *Spec) validateRenames() []Problem {
	var ps []Problem
	add := func(path, msg string) { ps = append(ps, Problem{Path: path, Message: msg}) }
	entityNames := map[string]bool{}
	for _, e := range s.Entities {
		entityNames[e.Name] = true
	}
	from := map[string]bool{}
	for i, e := range s.Entities {
		p := fmt.Sprintf("entities[%d]", i)
		if e.RenamedFrom != "" {
			switch {
			case !rePascal.MatchString(e.RenamedFrom):
				add(p+".renamed_from", "use the previous PascalCase entity name")
			case e.RenamedFrom == e.Name || entityNames[e.RenamedFrom]:
				add(p+".renamed_from", fmt.Sprintf("%q is still an entity of this spec", e.RenamedFrom))
			case from[e.RenamedFrom]:
				add(p+".renamed_from", fmt.Sprintf("two entities are renamed from %q", e.RenamedFrom))
			}
			from[e.RenamedFrom] = true
		}
		fieldNames := map[string]bool{}
		for _, f := range e.Fields {
			fieldNames[f.Name] = true
		}
		fieldFrom := map[string]bool{}
		for j, f := range e.Fields {
			if f.RenamedFrom == "" {
				continue
			}
			fp := fmt.Sprintf("%s.fields[%d].renamed_from", p, j)
			switch {
			case !reSnake.MatchString(f.RenamedFrom):
				add(fp, "use the previous lower_snake_case field name")
			case fieldNames[f.RenamedFrom]:
				add(fp, fmt.Sprintf("%q is still a field of this entity", f.RenamedFrom))
			case fieldFrom[f.RenamedFrom]:
				add(fp, fmt.Sprintf("two fields are renamed from %q", f.RenamedFrom))
			}
			fieldFrom[f.RenamedFrom] = true
		}
	}
	for i, m := range s.Methods {
		for _, side := range []struct {
			name   string
			fields []Field
		}{{"request", m.Request}, {"response", m.Response}} {
			for j, f := range side.fields {
				if f.RenamedFrom != "" {
					add(fmt.Sprintf("methods[%d].%s[%d].renamed_from", i, side.name, j), "renamed_from applies to entity fields only")
				}
			}
		}
	}
	return ps
}
