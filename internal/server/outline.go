package server

import (
	"slices"
	"strings"

	"github.com/tillknuesting/fliessband/internal/spec"
)

// outline is the service's shape for the UI's blueprint: the entities with
// their CRUD RPCs, the custom methods and which entities each one touches.
type outline struct {
	Entities []outlineEntity `json:"entities"`
	Methods  []outlineMethod `json:"methods"`
	Features []string        `json:"features"`
}

type outlineEntity struct {
	Name   string         `json:"name"`
	Doc    string         `json:"doc,omitempty"`
	Fields []outlineField `json:"fields"`
	RPCs   []string       `json:"rpcs"`
}

type outlineField struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Repeated bool   `json:"repeated,omitempty"`
	Required bool   `json:"required,omitempty"`
	Unique   bool   `json:"unique,omitempty"`
}

type outlineMethod struct {
	Name     string         `json:"name"`
	Doc      string         `json:"doc,omitempty"`
	Request  []outlineField `json:"request"`
	Response []outlineField `json:"response"`
	Touches  []string       `json:"touches"`
	Examples int            `json:"examples"`
	Holdout  int            `json:"holdout"`
}

func fields(fs []spec.Field) []outlineField {
	out := make([]outlineField, 0, len(fs))
	for _, f := range fs {
		out = append(out, outlineField{Name: f.Name, Type: f.Type, Repeated: f.Repeated, Required: f.Required, Unique: f.Unique})
	}
	return out
}

func buildOutline(s *spec.Spec) *outline {
	o := &outline{Entities: []outlineEntity{}, Methods: []outlineMethod{}, Features: []string{}}
	for _, e := range s.Entities {
		o.Entities = append(o.Entities, outlineEntity{Name: e.Name, Doc: e.Doc, Fields: fields(e.Fields), RPCs: e.RPCNames()})
	}
	for _, m := range s.Methods {
		om := outlineMethod{Name: m.Name, Doc: m.Doc, Request: fields(m.Request), Response: fields(m.Response), Touches: []string{}}
		touch := func(name string) {
			if !slices.Contains(om.Touches, name) {
				om.Touches = append(om.Touches, name)
			}
		}
		for _, e := range s.Entities {
			for _, f := range append(slices.Clone(m.Request), m.Response...) {
				if f.Type == e.Name {
					touch(e.Name)
				}
			}
			if strings.Contains(m.Name, e.Name) || (e.Plural != "" && strings.Contains(m.Name, e.Plural)) {
				touch(e.Name)
			}
			for _, ex := range m.Examples {
				if _, ok := ex.Given[e.Name]; ok {
					touch(e.Name)
				}
			}
		}
		for _, ex := range m.Examples {
			om.Examples++
			if ex.Holdout {
				om.Holdout++
			}
		}
		o.Methods = append(o.Methods, om)
	}
	f := s.Features
	for _, x := range []struct {
		name string
		ok   bool
	}{{"PostgreSQL", f.HasPostgres()}, {"Health", f.HasHealth()}, {"Reflection", f.HasReflection()}, {"OpenTelemetry", f.OTel}, {"Docker", f.HasDocker()}, {"Kubernetes", f.HasKubernetes()}, {"CI", f.HasCI()}} {
		if x.ok {
			o.Features = append(o.Features, x.name)
		}
	}
	return o
}
