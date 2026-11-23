// Package supply produces the supply-chain records of a factory run: a
// CycloneDX 1.6 software bill of materials read from the built binary's
// embedded build information, and an in-toto statement carrying SLSA v1
// provenance that ties the binary and every generated file to the spec,
// the generator, the tools and (by digest) the LLM prompts that made them.
//
// The statement is unsigned. In a release pipeline, sign it, for example
// with Sigstore cosign, so consumers can verify where the code came from.
package supply

import (
	"crypto/rand"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Module is one module linked into the binary.
type Module struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Sum     string `json:"sum,omitempty"`
}

// Build summarises a Go binary.
type Build struct {
	Main      Module
	GoVersion string
	Deps      []Module
	Settings  [][2]string
	SHA256    string
}

// ReadBuild reads the build information embedded in a Go binary.
func ReadBuild(binary string) (*Build, error) {
	bi, err := buildinfo.ReadFile(binary)
	if err != nil {
		return nil, err
	}
	digest, err := FileSHA256(binary)
	if err != nil {
		return nil, err
	}
	b := &Build{Main: Module{Path: bi.Main.Path, Version: bi.Main.Version}, GoVersion: bi.GoVersion, SHA256: digest}
	for _, d := range bi.Deps {
		if d.Replace != nil {
			d = d.Replace
		}
		b.Deps = append(b.Deps, Module{Path: d.Path, Version: d.Version, Sum: d.Sum})
	}
	for _, s := range bi.Settings {
		b.Settings = append(b.Settings, [2]string{s.Key, s.Value})
	}
	return b, nil
}

// FileSHA256 returns the hex SHA-256 of a file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func purl(path, version string) string {
	return "pkg:golang/" + path + "@" + version
}

// sumHex converts a go.sum "h1:" hash (base64 SHA-256) to hex.
func sumHex(sum string) string {
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sum, "h1:"))
	if err != nil || len(b) != sha256.Size {
		return ""
	}
	return hex.EncodeToString(b)
}

func uuidV4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// SBOM renders a CycloneDX 1.6 JSON document for the build.
func SBOM(b *Build, version string, now time.Time) ([]byte, error) {
	type hash struct {
		Alg     string `json:"alg"`
		Content string `json:"content"`
	}
	type component struct {
		Type    string `json:"type"`
		BOMRef  string `json:"bom-ref"`
		Name    string `json:"name"`
		Version string `json:"version"`
		PURL    string `json:"purl,omitempty"`
		Hashes  []hash `json:"hashes,omitempty"`
	}
	type property struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	mainVersion := b.Main.Version
	if mainVersion == "" || mainVersion == "(devel)" {
		mainVersion = version
	}
	main := component{Type: "application", BOMRef: purl(b.Main.Path, mainVersion), Name: b.Main.Path, Version: mainVersion,
		PURL: purl(b.Main.Path, mainVersion), Hashes: []hash{{"SHA-256", b.SHA256}}}
	comps := []component{{Type: "platform", BOMRef: "pkg:golang/stdlib@" + b.GoVersion, Name: "stdlib", Version: b.GoVersion,
		PURL: "pkg:golang/stdlib@" + strings.TrimPrefix(b.GoVersion, "go")}}
	var refs []string
	for _, d := range b.Deps {
		c := component{Type: "library", BOMRef: purl(d.Path, d.Version), Name: d.Path, Version: d.Version, PURL: purl(d.Path, d.Version)}
		if h := sumHex(d.Sum); h != "" {
			c.Hashes = []hash{{"SHA-256", h}}
		}
		comps = append(comps, c)
		refs = append(refs, c.BOMRef)
	}
	var props []property
	for _, s := range b.Settings {
		props = append(props, property{Name: "go:build:" + s[0], Value: s[1]})
	}
	doc := map[string]any{
		"bomFormat":    "CycloneDX",
		"specVersion":  "1.6",
		"serialNumber": "urn:uuid:" + uuidV4(),
		"version":      1,
		"metadata": map[string]any{
			"timestamp":  now.UTC().Format(time.RFC3339),
			"tools":      map[string]any{"components": []component{{Type: "application", BOMRef: "fliessband", Name: "fliessband", Version: version}}},
			"component":  main,
			"properties": props,
		},
		"components":   comps,
		"dependencies": []map[string]any{{"ref": main.BOMRef, "dependsOn": append([]string{comps[0].BOMRef}, refs...)}},
	}
	return json.MarshalIndent(doc, "", "  ")
}

// Subject is an artifact the provenance describes.
type Subject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

// Dependency is an input to the build that is not a parameter.
type Dependency struct {
	Name        string            `json:"name"`
	Digest      map[string]string `json:"digest,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Provenance describes how a run produced its outputs.
type Provenance struct {
	RunID      string
	Started    time.Time
	Finished   time.Time
	Subjects   []Subject
	External   map[string]any // the spec digest and run options
	Internal   map[string]any // generator version, templates digest, model, prompts
	Deps       []Dependency   // tools
	Byproducts []Subject      // e.g. the SBOM
}

// Statement renders an in-toto Statement v1 with a SLSA Provenance v1
// predicate.
func (p Provenance) Statement() ([]byte, error) {
	doc := map[string]any{
		"_type":         "https://in-toto.io/Statement/v1",
		"subject":       p.Subjects,
		"predicateType": "https://slsa.dev/provenance/v1",
		"predicate": map[string]any{
			"buildDefinition": map[string]any{
				"buildType":            "https://fliessband.local/factory-run/v1",
				"externalParameters":   p.External,
				"internalParameters":   p.Internal,
				"resolvedDependencies": p.Deps,
			},
			"runDetails": map[string]any{
				"builder": map[string]any{"id": "urn:fliessband:local"},
				"metadata": map[string]any{
					"invocationId": p.RunID,
					"startedOn":    p.Started.UTC().Format(time.RFC3339),
					"finishedOn":   p.Finished.UTC().Format(time.RFC3339),
				},
				"byproducts": p.Byproducts,
			},
		},
	}
	return json.MarshalIndent(doc, "", "  ")
}

// SHA256 returns the hex SHA-256 of b.
func SHA256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
