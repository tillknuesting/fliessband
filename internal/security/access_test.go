package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tillknuesting/fliessband/internal/gen"
	"github.com/tillknuesting/fliessband/internal/metrics"
	"github.com/tillknuesting/fliessband/internal/spec"
)

// renderSpec generates the service for a spec source into a temporary
// directory.
func renderSpec(t *testing.T, src string) (string, *spec.Spec) {
	t.Helper()
	s, ps, err := spec.Parse([]byte(src))
	if err != nil || spec.HasErrors(ps) {
		t.Fatalf("spec: %v %v", err, ps)
	}
	files, err := gen.Generate(s)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := gen.WriteFiles(root, files); err != nil {
		t.Fatal(err)
	}
	return root, s
}

func bookings(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../specs/bookings.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func accessChecks(t *testing.T, root string, s *spec.Spec) map[string]metrics.SecurityCheck {
	t.Helper()
	got := status(Audit(root, s, &metrics.Report{}, func(string) bool { return true }))
	for id, c := range got {
		if c.Area != accessArea {
			delete(got, id)
		}
	}
	return got
}

func TestAccessControlWithoutAuth(t *testing.T) {
	root, s := service(t)
	got := accessChecks(t, root, s)
	want := map[string]string{"authn": "warn", "authz-wired": "skip", "rpc-rules": "skip", "jwt-verify": "skip", "token-logs": "skip"}
	for id, st := range want {
		if got[id].Status != st {
			t.Errorf("%s = %s (%s), want %s", id, got[id].Status, got[id].Detail, st)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d access checks, want %d", len(got), len(want))
	}
	if !strings.Contains(got["authn"].Detail, "mTLS") {
		t.Errorf("the warning should explain why mTLS alone is not access control: %s", got["authn"].Detail)
	}
}

func TestAccessControlWithAuth(t *testing.T) {
	root, s := renderSpec(t, bookings(t))
	got := accessChecks(t, root, s)
	for _, id := range []string{"authn", "authz-wired", "rpc-rules", "jwt-verify", "token-logs"} {
		c := got[id]
		if c.Status != "pass" {
			t.Errorf("%s = %s (%s), want pass", id, c.Status, c.Detail)
		}
		if c.File != "" && c.File != "spec.yaml" && c.Line == 0 {
			t.Errorf("%s passes without citing a line in %s", id, c.File)
		}
	}
}

func TestAccessControlFindsGaps(t *testing.T) {
	t.Run("custom method public by default", func(t *testing.T) {
		src := strings.Replace(bookings(t), "default: authenticated", "default: public", 1)
		src = strings.Replace(src, "    access: [guest, frontdesk, admin]\n", "", 1)
		root, s := renderSpec(t, src)
		if c := accessChecks(t, root, s)["rpc-rules"]; c.Status != "fail" || !strings.Contains(c.Detail, "CheckAvailability") {
			t.Errorf("rpc-rules = %+v, want a fail naming CheckAvailability", c)
		}
	})
	t.Run("operation public by default", func(t *testing.T) {
		src := strings.Replace(bookings(t), "default: authenticated", "default: public", 1)
		src = strings.Replace(src, ", list: [frontdesk]", "", 1)
		root, s := renderSpec(t, src)
		if c := accessChecks(t, root, s)["rpc-rules"]; c.Status != "warn" || !strings.Contains(c.Detail, "ListReservations") {
			t.Errorf("rpc-rules = %+v, want a warning naming ListReservations", c)
		}
	})
	t.Run("explicitly public method", func(t *testing.T) {
		src := strings.Replace(bookings(t), "access: [guest, frontdesk, admin]", "access: public", 1)
		src = strings.Replace(src, "        as: []              # signed in, but without a role\n", "", 1)
		root, s := renderSpec(t, src)
		if c := accessChecks(t, root, s)["rpc-rules"]; c.Status != "pass" {
			t.Errorf("rpc-rules = %+v, want pass for a method declared public", c)
		}
	})

	root, s := renderSpec(t, bookings(t))
	edit := func(rel, old, new string) {
		t.Helper()
		p := filepath.Join(root, rel)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), old) {
			t.Fatalf("%s does not contain %q", rel, old)
		}
		if err := os.WriteFile(p, []byte(strings.Replace(string(b), old, new, 1)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	edit("internal/auth/auth.go", "jwt.WithAudience(cfg.Audience),", "")
	edit("internal/auth/auth.go", `return &Principal{Subject: sub,`, `log.Info("verified", "token", raw)
	return &Principal{Subject: sub,`)
	edit("internal/auth/rules.go", `"/acme.bookings.v1.BookingsService/ListRooms":`, `"/acme.bookings.v1.BookingsService/Gone":`)
	edit("internal/server/server.go", "o.Auth.Unary(), ", "")
	got := accessChecks(t, root, s)
	for id, want := range map[string]string{"jwt-verify": "the audience", "token-logs": "", "rpc-rules": "ListRooms", "authz-wired": ""} {
		if c := got[id]; c.Status != "fail" || !strings.Contains(c.Detail, want) {
			t.Errorf("%s = %+v, want a fail mentioning %q", id, c, want)
		}
	}
	if c := got["token-logs"]; c.File != "internal/auth/auth.go" || c.Line == 0 {
		t.Errorf("token-logs should cite the log call, got %s:%d", c.File, c.Line)
	}
}

func TestLogsCredentials(t *testing.T) {
	for src, want := range map[string]bool{
		`log.Info("token rejected", "reason", why)`:          false,
		`log.Info("rejected", "token", raw)`:                 true,
		"s.log.WarnContext(ctx, `bad header`, \"h\", md)":    true,
		`slog.Debug("auth", "authorization", vals[0])`:       true,
		`log.Error("JWKS", "url", u, "err", err)`:            false,
		`fmt.Println(token)`:                                 false,
		`logger.Info("x", "sub", p.Subject, "roles", roles)`: false,
	} {
		if got := logsCredentials(src) > 0; got != want {
			t.Errorf("logsCredentials(%s) = %v, want %v", src, got, want)
		}
	}
}
