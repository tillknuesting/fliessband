package server

import (
	"os"
	"slices"
	"testing"

	"github.com/tillknuesting/fliessband/internal/spec"
)

func TestOutline(t *testing.T) {
	src, err := os.ReadFile("../../specs/inventory.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, _, err := spec.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	o := buildOutline(s)
	if len(o.Entities) != 2 || len(o.Entities[0].RPCs) != 5 {
		t.Fatalf("entities = %+v", o.Entities)
	}
	for _, m := range o.Methods {
		if m.Name == "AdjustStock" && (!slices.Contains(m.Touches, "Product") || m.Examples == 0) {
			t.Errorf("AdjustStock = %+v, want it to touch Product and carry examples", m)
		}
	}
	if !slices.Contains(o.Features, "PostgreSQL") {
		t.Errorf("features = %v", o.Features)
	}
}
