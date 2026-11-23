package spec

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// RefPattern matches a reference to a seeded record, ${Entity.index.field}.
var RefPattern = regexp.MustCompile(`\$\{([A-Z][A-Za-z0-9]*)\.([0-9]+)\.([a-z][a-z0-9_]*)\}`)

// SystemFields are the fields every entity gets from the factory.
var SystemFields = []Field{
	{Name: "id", Type: "string"},
	{Name: "create_time", Type: "timestamp"},
	{Name: "update_time", Type: "timestamp"},
}

// Entity returns the entity with the given name, or nil.
func (s *Spec) Entity(name string) *Entity {
	for i := range s.Entities {
		if s.Entities[i].Name == name {
			return &s.Entities[i]
		}
	}
	return nil
}

// AllFields returns the entity's fields followed by the system fields.
func (e Entity) AllFields() []Field {
	return append(slices.Clone(e.Fields), SystemFields...)
}

func fieldIn(fields []Field, name string) *Field {
	for i := range fields {
		if fields[i].Name == name {
			return &fields[i]
		}
	}
	return nil
}

func (s *Spec) entityIndex(name string) int {
	return slices.IndexFunc(s.Entities, func(e Entity) bool { return e.Name == name })
}

// GivenOrder returns the entities of an example's given block in seeding
// order, which is the order the entities are declared in the spec.
func (s *Spec) GivenOrder(ex Example) []string {
	var out []string
	for _, e := range s.Entities {
		if _, ok := ex.Given[e.Name]; ok {
			out = append(out, e.Name)
		}
	}
	return out
}

func (s *Spec) validateExample(p string, m Method, ex Example) []Problem {
	var ps []Problem
	add := func(path, msg string) { ps = append(ps, Problem{Path: path, Message: msg}) }

	var names []string
	for _, e := range s.Entities {
		names = append(names, e.Name)
	}
	for name, recs := range ex.Given {
		e := s.Entity(name)
		if e == nil {
			add(p+".given."+name, fmt.Sprintf("unknown entity; use one of %s", strings.Join(names, ", ")))
			continue
		}
		for i, rec := range recs {
			rp := fmt.Sprintf("%s.given.%s[%d]", p, name, i)
			for _, k := range sortedKeys(rec) {
				f := fieldIn(e.Fields, k)
				if f == nil {
					add(rp+"."+k, fmt.Sprintf("not a field of %s (id and timestamps are set by the service)", name))
					continue
				}
				if msg := s.checkValue(*f, rec[k]); msg != "" {
					add(rp+"."+k, msg)
				}
				for _, ref := range refsIn(rec[k]) {
					if msg := s.checkRef(ref, ex.Given, name, i); msg != "" {
						add(rp+"."+k, msg)
					}
				}
			}
		}
	}
	for _, k := range sortedKeys(ex.Request) {
		f := fieldIn(m.Request, k)
		if f == nil {
			add(p+".request."+k, fmt.Sprintf("not a request field of %s", m.Name))
			continue
		}
		if msg := s.checkValue(*f, ex.Request[k]); msg != "" {
			add(p+".request."+k, msg)
		}
		for _, ref := range refsIn(ex.Request[k]) {
			if msg := s.checkRef(ref, ex.Given, "", 0); msg != "" {
				add(p+".request."+k, msg)
			}
		}
	}
	if ex.Error != "" {
		if !slices.Contains(StatusCodes, ex.Error) {
			add(p+".error", fmt.Sprintf("unknown gRPC code %q; use one of %s", ex.Error, strings.Join(StatusCodes, ", ")))
		}
		if len(ex.Expect) > 0 {
			add(p+".expect", "an example expects either an error or response fields, not both")
		}
	}
	for _, path := range sortedKeys(ex.Expect) {
		if msg := s.checkPath(m.Response, path); msg != "" {
			add(p+".expect."+path, msg)
		}
		for _, ref := range refsIn(ex.Expect[path]) {
			if msg := s.checkRef(ref, ex.Given, "", 0); msg != "" {
				add(p+".expect."+path, msg)
			}
		}
	}
	return ps
}

// checkRef validates a reference. Inside given records (inEntity != "") a
// reference may only point at records seeded earlier.
func (s *Spec) checkRef(ref []string, given map[string][]map[string]any, inEntity string, inIndex int) string {
	ent, idx, field := ref[1], ref[2], ref[3]
	recs, ok := given[ent]
	if !ok {
		return fmt.Sprintf("${%s.%s.%s}: the example seeds no %s", ent, idx, field, ent)
	}
	i, _ := strconv.Atoi(idx)
	if i >= len(recs) {
		return fmt.Sprintf("${%s.%s.%s}: the example seeds only %d %s", ent, idx, field, len(recs), ent)
	}
	if e := s.Entity(ent); e == nil || fieldIn(e.AllFields(), field) == nil {
		return fmt.Sprintf("${%s.%s.%s}: %s has no field %q", ent, idx, field, ent, field)
	}
	if inEntity != "" {
		earlier := s.entityIndex(ent) < s.entityIndex(inEntity) || (ent == inEntity && i < inIndex)
		if !earlier {
			return fmt.Sprintf("${%s.%s.%s}: records are seeded in the order entities are declared, so a %s can only refer to records seeded before it", ent, idx, field, inEntity)
		}
	}
	return ""
}

// checkValue reports a problem if v cannot be a value of field f.
func (s *Spec) checkValue(f Field, v any) string {
	if f.Repeated {
		list, ok := v.([]any)
		if !ok {
			return fmt.Sprintf("%s is a repeated field: use a list", f.Name)
		}
		g := f
		g.Repeated = false
		for _, el := range list {
			if msg := s.checkValue(g, el); msg != "" {
				return msg
			}
		}
		return ""
	}
	if str, ok := v.(string); ok && RefPattern.MatchString(str) {
		return "" // resolved when the example runs
	}
	if e := s.Entity(f.Type); e != nil {
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s is a %s: use a map of its fields", f.Name, f.Type)
		}
		for _, k := range sortedKeys(obj) {
			sub := fieldIn(e.AllFields(), k)
			if sub == nil {
				return fmt.Sprintf("%s has no field %q", f.Type, k)
			}
			if msg := s.checkValue(*sub, obj[k]); msg != "" {
				return msg
			}
		}
		return ""
	}
	switch f.Type {
	case "string", "bytes":
		if _, ok := v.(string); !ok {
			return fmt.Sprintf("%s is a %s: quote the value", f.Name, f.Type)
		}
	case "bool":
		if _, ok := v.(bool); !ok {
			return fmt.Sprintf("%s is a bool: use true or false", f.Name)
		}
	case "timestamp":
		switch t := v.(type) {
		case time.Time:
		case string:
			if _, err := time.Parse(time.RFC3339Nano, t); err != nil {
				return fmt.Sprintf("%s is a timestamp: use RFC 3339, e.g. 2026-03-01T15:00:00Z", f.Name)
			}
		default:
			return fmt.Sprintf("%s is a timestamp: use RFC 3339, e.g. 2026-03-01T15:00:00Z", f.Name)
		}
	default: // numbers
		n, ok := number(v)
		if !ok {
			return fmt.Sprintf("%s is a %s: use a number", f.Name, f.Type)
		}
		if IsInteger(f.Type) {
			lo, hi := intRange(f.Type)
			if n != math.Trunc(n) || n < lo || n > hi {
				return fmt.Sprintf("%s is a %s: %v does not fit", f.Name, f.Type, v)
			}
		}
	}
	return ""
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

// checkPath validates a dotted expect path against the response fields.
func (s *Spec) checkPath(fields []Field, path string) string {
	segs := strings.Split(path, ".")
	cur := fields
	for i := 0; i < len(segs); i++ {
		f := fieldIn(cur, segs[i])
		if f == nil {
			var names []string
			for _, c := range cur {
				names = append(names, c.Name)
			}
			return fmt.Sprintf("%q is not a field here; use one of %s", segs[i], strings.Join(names, ", "))
		}
		if f.Repeated {
			if i+1 == len(segs) {
				return "" // the whole list, compared as JSON
			}
			i++
			if segs[i] == "#" {
				if i+1 != len(segs) {
					return "# (the length of a list) must end the path"
				}
				return ""
			}
			if _, err := strconv.Atoi(segs[i]); err != nil {
				return fmt.Sprintf("%s is a list: follow it with an index (0, 1, …) or #", f.Name)
			}
		}
		if e := s.Entity(f.Type); e != nil {
			if i+1 == len(segs) {
				return "" // the whole message, compared as JSON
			}
			cur = e.AllFields()
			continue
		}
		if i+1 < len(segs) {
			return fmt.Sprintf("%s is a %s and has no fields", f.Name, f.Type)
		}
	}
	return ""
}

// refsIn returns the ${...} references inside v, as RefPattern submatches.
func refsIn(v any) [][]string {
	var out [][]string
	switch t := v.(type) {
	case string:
		out = append(out, RefPattern.FindAllStringSubmatch(t, -1)...)
	case []any:
		for _, el := range t {
			out = append(out, refsIn(el)...)
		}
	case map[string]any:
		for _, k := range sortedKeys(t) {
			out = append(out, refsIn(t[k])...)
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
