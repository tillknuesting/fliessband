package gen

import (
	"bytes"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/tillknuesting/fliessband/internal/spec"
)

func renderFile(t *testing.T, path string) map[string]string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s, ps, err := spec.Parse(src)
	if err != nil || spec.HasErrors(ps) {
		t.Fatalf("spec: %v %v", err, ps)
	}
	files, err := Generate(s)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Generate(s)
	if err != nil || len(again) != len(files) {
		t.Fatalf("second render: %v", err)
	}
	out := map[string]string{}
	for i, f := range files {
		if !bytes.Equal(f.Data, again[i].Data) {
			t.Errorf("%s: rendering is not deterministic", f.Path)
		}
		if strings.HasSuffix(f.Path, ".go") {
			if _, err := parser.ParseFile(token.NewFileSet(), f.Path, f.Data, 0); err != nil {
				t.Errorf("%s: %v", f.Path, err)
			}
		}
		if bytes.Contains(f.Data, []byte("<no value>")) {
			t.Errorf("%s: a template field rendered as <no value>", f.Path)
		}
		out[f.Path] = string(f.Data)
	}
	return out
}

// TestAuthRenders renders the auth example and the auth variants: JWT with
// roles (bookings), client certificates only, and JWT with a public default
// and nested roles claim.
func TestAuthRenders(t *testing.T) {
	files := renderFile(t, "../../specs/bookings.yaml")
	for path, want := range map[string][]string{
		"internal/auth/rules.go": {
			`"/acme.bookings.v1.BookingsService/CreateRoom":        Roles("admin"),`,
			`"/acme.bookings.v1.BookingsService/ListRooms":         Public,`,
			`"/acme.bookings.v1.BookingsService/CheckAvailability": Roles("guest", "frontdesk", "admin"),`,
			`var DefaultRule = Authenticated`,
			`var AllRoles = []string{"admin", "frontdesk", "guest"}`,
			`DefaultJWKSURL  = "https://idp.example.com/.well-known/jwks.json"`,
		},
		"internal/auth/auth.go":               {`jwt.WithValidMethods([]string{"RS256", "ES256"})`, `jwt.WithIssuer(cfg.Issuer)`, `jwt.WithAudience(cfg.Audience)`, `jwt.WithExpirationRequired()`},
		"internal/auth/principal.go":          {"func FromContext(ctx context.Context) (p *Principal, ok bool)"},
		"internal/server/server.go":           {"o.Auth.Unary()", "o.Auth.Stream()", `"github.com/acme/bookings/internal/auth"`},
		"cmd/server/main.go":                  {"opts.Auth = auth.New(ctx, auth.Config{"},
		"internal/config/auth.go":             {`p.str("AUTH_JWKS_URL", &c.JWKSURL)`},
		"internal/service/service_test.go":    {"grpc.NewServer(testAuth(t)...)", "grpc.WithChainUnaryInterceptor(defaultCaller)"},
		"internal/service/acceptance_test.go": {"ctx = asRoles(ctx, t, ex.as...)", `hasAs:   true, as: nil,`},
		"go.mod":                              {KeyfuncModule + " " + KeyfuncVer, JWTModule + " " + JWTVersion},
		"README.md":                           {"## Access control", "`AUTH_JWKS_URL`", "| `CreateRoom` | roles `admin` |"},
	} {
		for _, w := range want {
			if !strings.Contains(files[path], w) {
				t.Errorf("%s lacks %s", path, w)
			}
		}
	}
	if !strings.Contains(files["internal/auth/auth_test.go"], `{"/acme.bookings.v1.BookingsService/CreateRoom", "role guest", true, []string{"guest"}, codes.PermissionDenied}`) {
		t.Error("the authorisation table lacks the guest case for CreateRoom")
	}

	mtls := renderFile(t, "testdata/auth_mtls.yaml")
	if _, ok := mtls["internal/config/auth.go"]; ok {
		t.Error("a service without JWT needs no token configuration")
	}
	for _, s := range []string{mtls["internal/auth/auth.go"], mtls["go.mod"]} {
		if strings.Contains(s, "jwt") {
			t.Error("a service without JWT must not depend on a JWT library")
		}
	}
	if !strings.Contains(mtls["internal/auth/rules.go"], "var DefaultRule = Deny") || !strings.Contains(mtls["internal/auth/rules.go"], "const MTLSIdentity = true") {
		t.Errorf("mTLS rules:\n%s", mtls["internal/auth/rules.go"])
	}

	public := renderFile(t, "testdata/auth_public.yaml")
	if !strings.Contains(public["internal/auth/rules.go"], `"/notes.v1.NotesService/Archive":    Public, // the default`) {
		t.Errorf("implicit rules must be marked:\n%s", public["internal/auth/rules.go"])
	}
	if !strings.Contains(public["internal/auth/rules.go"], `RolesClaim = "realm_access.roles"`) {
		t.Error("the nested roles claim is not carried over")
	}
}

// TestNoAuthNoAuthCode checks that specs without auth get none of it.
func TestNoAuthNoAuthCode(t *testing.T) {
	files := renderFile(t, "../../specs/inventory.yaml")
	for path, data := range files {
		if strings.HasPrefix(path, "internal/auth/") || path == "internal/config/auth.go" || strings.HasSuffix(path, "/auth_test.go") {
			t.Errorf("%s generated without auth", path)
		}
		if strings.Contains(data, "internal/auth") || strings.Contains(data, "golang-jwt") {
			t.Errorf("%s refers to auth without an auth block", path)
		}
	}
}
