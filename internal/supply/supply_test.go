package supply

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSBOMFromOwnBinary(t *testing.T) {
	exe, err := os.Executable() // the test binary has build information
	if err != nil {
		t.Fatal(err)
	}
	b, err := ReadBuild(exe)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.GoVersion, "go") || len(b.SHA256) != 64 {
		t.Fatalf("build info: %+v", b)
	}
	b.Deps = append(b.Deps, Module{Path: "example.com/dep", Version: "v1.2.3", Sum: "h1:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU="})
	out, err := SBOM(b, "test", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		BOMFormat   string `json:"bomFormat"`
		SpecVersion string `json:"specVersion"`
		Components  []struct {
			Name   string `json:"name"`
			PURL   string `json:"purl"`
			Hashes []struct{ Alg, Content string }
		} `json:"components"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.BOMFormat != "CycloneDX" || doc.SpecVersion != "1.6" {
		t.Fatalf("not a CycloneDX 1.6 document: %s", out[:200])
	}
	var found bool
	for _, c := range doc.Components {
		if c.Name == "example.com/dep" {
			found = c.PURL == "pkg:golang/example.com/dep@v1.2.3" && len(c.Hashes) == 1 && len(c.Hashes[0].Content) == 64
		}
	}
	if !found {
		t.Errorf("dependency missing or without purl and hash: %s", out)
	}
}

func TestStatement(t *testing.T) {
	p := Provenance{RunID: "r1", Started: time.Now(), Finished: time.Now(),
		Subjects: []Subject{{Name: "bin/server", Digest: map[string]string{"sha256": SHA256([]byte("x"))}}},
		External: map[string]any{"spec": map[string]any{"sha256": SHA256([]byte("spec"))}},
		Deps:     []Dependency{{Name: "buf", Annotations: map[string]string{"version": "1.75.0"}}}}
	out, err := p.Statement()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["_type"] != "https://in-toto.io/Statement/v1" || doc["predicateType"] != "https://slsa.dev/provenance/v1" {
		t.Fatalf("wrong statement types: %s", out)
	}
}
