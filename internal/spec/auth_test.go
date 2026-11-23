package spec

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

const authBase = `name: svc
auth:
  jwt: {issuer: "https://idp.test", audience: s, jwks_url: "https://idp.test/jwks"}
entities:
  - name: A
    fields: [{name: x, type: string}]
`

func TestAuthParses(t *testing.T) {
	src := authBase + `    access: {create: [admin, editor], get: public, delete: deny}
methods:
  - name: Do
    doc: x
    access: [editor]
    examples:
      - {name: no role, as: [], error: PERMISSION_DENIED}
      - {name: editor, as: [editor]}
      - {name: everyone}
`
	s, ps, err := Parse([]byte(src))
	if err != nil || HasErrors(ps) {
		t.Fatalf("%v %v", err, ps)
	}
	if s.Auth.Default != AccessAuthenticated || s.Auth.RolesClaim != DefaultRolesClaim {
		t.Errorf("defaults not applied: %+v", s.Auth)
	}
	if got := s.Roles(); !slices.Equal(got, []string{"admin", "editor"}) {
		t.Errorf("roles = %v", got)
	}
	e := s.Entities[0]
	for op, want := range map[string]string{"create": "roles admin, editor", "get": "public", "delete": "deny", "list": "authenticated", "update": "authenticated"} {
		a, own := s.OpAccess(e, op)
		if a.String() != want || own != (op != "list" && op != "update") {
			t.Errorf("%s: %s (own %v), want %s", op, a, own, want)
		}
	}
	if a, own := s.MethodAccess(s.Methods[0]); a.String() != "roles editor" || !own {
		t.Errorf("method rule = %s, %v", a, own)
	}
	ex := s.Methods[0].Examples
	if ex[0].As == nil || len(ex[0].As) != 0 || !slices.Equal(ex[1].As, []string{"editor"}) || ex[2].As != nil {
		t.Errorf("as = %#v, %#v, %#v; want empty, [editor], unset", ex[0].As, ex[1].As, ex[2].As)
	}
}

func TestAccessJSON(t *testing.T) {
	for _, tt := range []struct {
		in   Access
		json string
	}{{Access{Kind: AccessPublic}, `"public"`}, {Access{Kind: AccessRoles, Roles: []string{"a", "b"}}, `["a","b"]`}} {
		b, err := json.Marshal(tt.in)
		if err != nil || string(b) != tt.json {
			t.Errorf("marshal %v = %s, %v; want %s", tt.in, b, err, tt.json)
		}
		var back Access
		if err := json.Unmarshal(b, &back); err != nil || back.String() != tt.in.String() {
			t.Errorf("round trip of %s = %v, %v", b, back, err)
		}
	}
	if err := json.Unmarshal([]byte(`{}`), new(Access)); err == nil {
		t.Error("an object is not an access rule")
	}
}

func TestNoAuthIsOpen(t *testing.T) {
	s, ps, err := Parse([]byte("name: svc\nentities: [{name: A, fields: [{name: x, type: string}]}]\n"))
	if err != nil || HasErrors(ps) {
		t.Fatalf("%v %v", err, ps)
	}
	if a, own := s.OpAccess(s.Entities[0], "get"); a.Kind != AccessPublic || own {
		t.Errorf("without auth every RPC is public, got %s", a)
	}
}

func TestAuthRejectsBadSpecs(t *testing.T) {
	noJWT := strings.Replace(authBase, `  jwt: {issuer: "https://idp.test", audience: s, jwks_url: "https://idp.test/jwks"}`, "  mtls_identity: true", 1)
	tests := []struct {
		name, yaml, wantPath string
	}{
		{"no identity source", "name: svc\nauth: {default: deny}\nentities: [{name: A, fields: [{name: x, type: string}]}]\n", "auth"},
		{"no issuer", strings.Replace(authBase, `issuer: "https://idp.test", `, "", 1), "auth.jwt.issuer"},
		{"no audience", strings.Replace(authBase, `audience: s, `, "", 1), "auth.jwt.audience"},
		{"no jwks url", strings.Replace(authBase, `, jwks_url: "https://idp.test/jwks"`, "", 1), "auth.jwt.jwks_url"},
		{"plain http jwks", strings.Replace(authBase, `https://idp.test/jwks`, `http://idp.test/jwks`, 1), "auth.jwt.jwks_url"},
		{"relative jwks", strings.Replace(authBase, `https://idp.test/jwks`, `/jwks`, 1), "auth.jwt.jwks_url"},
		{"bad roles claim", strings.Replace(authBase, "auth:\n", "auth:\n  roles_claim: \"a..b\"\n", 1), "auth.roles_claim"},
		{"bad default", strings.Replace(authBase, "auth:\n", "auth:\n  default: maybe\n", 1), "auth.default"},
		{"unknown op", authBase + "    ops: [get]\n    access: {create: public}\n", "entities[0].access.create"},
		{"unknown rule", authBase + "    access: {get: everyone}\n", "entities[0].access.get"},
		{"empty roles", authBase + "    access: {get: []}\n", "entities[0].access.get"},
		{"rule word as role", authBase + "    access: {get: [public]}\n", "entities[0].access.get"},
		{"bad role name", authBase + "    access: {get: [\"two words\"]}\n", "entities[0].access.get"},
		{"duplicate role", authBase + "    access: {get: [a, a]}\n", "entities[0].access.get"},
		{"access without auth", "name: svc\nentities: [{name: A, access: {get: public}, fields: [{name: x, type: string}]}]\n", "entities[0].access"},
		{"method access without auth", "name: svc\nmethods: [{name: Do, doc: x, access: public}]\n", "methods[0].access"},
		{"example caller without auth", "name: svc\nmethods: [{name: Do, doc: x, examples: [{name: e, as: [a]}]}]\n", "methods[0].examples[0].as"},
		{"example caller without jwt", noJWT + "methods: [{name: Do, doc: x, examples: [{name: e, as: []}]}]\n", "methods[0].examples[0].as"},
		{"roles without jwt", noJWT + "    access: {get: [admin]}\n", "auth.jwt"},
		{"bad example role", authBase + "methods: [{name: Do, doc: x, examples: [{name: e, as: [\"x y\"]}]}]\n", "methods[0].examples[0].as"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ps, err := Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range ps {
				if !p.Warning && strings.HasPrefix(p.Path, tt.wantPath) {
					return
				}
			}
			t.Fatalf("no error at %s; got %v", tt.wantPath, ps)
		})
	}
}

func TestAuthWarnings(t *testing.T) {
	src := strings.Replace(authBase, "auth:\n", "auth:\n  default: public\n", 1) +
		"methods: [{name: Do, doc: x, examples: [{name: e, as: [nobody]}]}]\n"
	_, ps, err := Parse([]byte(src))
	if err != nil || HasErrors(ps) {
		t.Fatalf("%v %v", err, ps)
	}
	var paths []string
	for _, p := range ps {
		if p.Warning {
			paths = append(paths, p.Path)
		}
	}
	for _, want := range []string{"auth.default", "methods[0].examples[0].as"} {
		if !slices.Contains(paths, want) {
			t.Errorf("no warning at %s; got %v", want, paths)
		}
	}
}

func TestAccessRuleShape(t *testing.T) {
	if _, _, err := Parse([]byte(authBase + "    access: {get: {roles: [a]}}\n")); err == nil {
		t.Error("an object as an access rule must be a parse error")
	}
}
