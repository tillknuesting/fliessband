package security

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tillknuesting/fliessband/internal/gen"
	"github.com/tillknuesting/fliessband/internal/metrics"
	"github.com/tillknuesting/fliessband/internal/spec"
)

func service(t *testing.T) (string, *spec.Spec) {
	t.Helper()
	src, err := os.ReadFile("../../specs/inventory.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, _, err := spec.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	files, err := gen.Generate(s)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, f.Data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root, s
}

func status(sec *metrics.Security) map[string]metrics.SecurityCheck {
	out := map[string]metrics.SecurityCheck{}
	for _, c := range sec.Checks {
		out[c.ID] = c
	}
	return out
}

func TestAuditGeneratedService(t *testing.T) {
	root, s := service(t)
	got := status(Audit(root, s, &metrics.Report{}, func(string) bool { return true }))
	want := map[string]string{
		"tls13": "pass", "mtls": "pass", "tls-default": "warn", "reflection": "warn",
		"max-msg": "pass", "shed": "pass", "deadline": "pass", "recovery": "pass",
		"log-payloads": "pass", "distroless": "pass", "nonroot": "pass", "digest": "warn",
		"k8s-nonroot": "pass", "k8s-rofs": "pass", "k8s-escalation": "pass", "k8s-caps": "pass",
		"k8s-seccomp": "pass", "k8s-token": "pass", "k8s-limits": "pass",
		"ci-permissions": "pass", "ci-pinned": "warn", "gosec": "pass", "secrets": "pass",
		"policy": "pass", "govulncheck": "pass", "fuzz": "skip", "sbom": "warn", "sandbox": "warn",
	}
	for id, st := range want {
		c, ok := got[id]
		if !ok {
			t.Errorf("no check %q", id)
			continue
		}
		if c.Status != st {
			t.Errorf("%s = %s (%s), want %s", id, c.Status, c.Detail, st)
		}
		if c.Status == "pass" && c.File != "" && c.File != "spec.yaml" && c.Line == 0 {
			t.Errorf("%s passes without citing a line in %s", id, c.File)
		}
	}
}

func TestAuditCatchesSecretsAndPayloadLogs(t *testing.T) {
	root, s := service(t)
	key := "-----BEGIN RSA " + "PRIVATE KEY-----\n"
	if err := os.WriteFile(filepath.Join(root, "internal/service/key.go"), []byte("package service\n\nconst k = `"+key+"`\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ic := filepath.Join(root, "internal/server/interceptors.go")
	b, _ := os.ReadFile(ic)
	b = []byte(string(b) + "\nfunc Logging2() {}\n")
	b = []byte(replaceOnce(string(b), `"code", code.String(),`, `"code", code.String(), "body", req,`))
	if err := os.WriteFile(ic, b, 0o600); err != nil {
		t.Fatal(err)
	}
	got := status(Audit(root, s, &metrics.Report{}, func(string) bool { return false }))
	if c := got["secrets"]; c.Status != "fail" || c.File != "internal/service/key.go" || c.Line != 3 {
		t.Errorf("secrets = %+v, want a fail at internal/service/key.go:3", c)
	}
	if c := got["log-payloads"]; c.Status != "fail" {
		t.Errorf("log-payloads = %+v, want fail", c)
	}
	if c := got["govulncheck"]; c.Status != "skip" {
		t.Errorf("govulncheck = %+v, want skip when the station did not run", c)
	}
}

func replaceOnce(s, old, new string) string {
	for i := 0; i+len(old) <= len(s); i++ {
		if s[i:i+len(old)] == old {
			return s[:i] + new + s[i+len(old):]
		}
	}
	return s
}

func TestAuditSandbox(t *testing.T) {
	root, s := service(t)
	for _, c := range []struct {
		sb   *metrics.Sandbox
		want string
	}{
		{nil, "warn"},
		{&metrics.Sandbox{Mode: "none"}, "warn"},
		{&metrics.Sandbox{Mode: "docker", Contained: true, Engine: "docker", Image: "golang:1.27.4", Runtime: "runsc", Network: "none"}, "pass"},
	} {
		rep := &metrics.Report{Factory: metrics.Factory{Sandbox: c.sb}}
		got := status(Audit(root, s, rep, func(string) bool { return true }))["sandbox"]
		if got.Status != c.want || got.Area != "Factory" {
			t.Errorf("sandbox %+v: %+v, want %s", c.sb, got, c.want)
		}
	}
}
