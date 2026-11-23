package gen

import (
	"strconv"
	"strings"

	"github.com/tillknuesting/fliessband/internal/spec"
)

// JWT and JWKS libraries of generated services with auth, pinned so every
// run builds against the same, reviewed versions. golang-jwt verifies the
// tokens; keyfunc fetches, caches and refreshes the provider's key set.
const (
	JWTModule     = "github.com/golang-jwt/jwt/v5"
	JWTVersion    = "v5.3.1"
	KeyfuncModule = "github.com/MicahParks/keyfunc/v3"
	KeyfuncVer    = "v3.9.2"
)

// AuthModel is the auth block of a spec, ready for the templates.
type AuthModel struct {
	JWT          bool
	Issuer       string
	Audience     string
	JWKSURL      string
	RolesClaim   string
	MTLSIdentity bool
	Default      string // Go expression of the default rule
	DefaultKind  string // public, authenticated or deny
	Rules        []*RuleModel
	Roles        []string // every role the spec names
	Cases        []AuthCase
}

// RuleModel is the access rule of one RPC.
type RuleModel struct {
	RPC      string // CreateRoom
	Method   string // /acme.bookings.v1.BookingsService/CreateRoom
	Expr     string // Go expression: Public, Authenticated, Deny or Roles("admin")
	Kind     string
	Roles    []string
	Explicit bool // named in the spec rather than the default
	Custom   bool // a custom method rather than a CRUD operation
}

// AuthCase is one row of the generated authorisation test: a caller and
// the code the interceptor must answer for one RPC.
type AuthCase struct {
	Method string
	Caller string // anonymous, no roles, all roles or role <name>
	Token  bool   // false sends no token
	Roles  string // Go expression of the caller's roles
	Code   string // codes.X
}

// PublicRPC returns the first RPC open to anonymous callers, or nil.
func (a *AuthModel) PublicRPC() *RuleModel {
	for _, r := range a.Rules {
		if r.Kind == spec.AccessPublic {
			return r
		}
	}
	return nil
}

// RoleRPC returns the first RPC that needs a role, or nil.
func (a *AuthModel) RoleRPC() *RuleModel {
	for _, r := range a.Rules {
		if r.Kind == spec.AccessRoles {
			return r
		}
	}
	return nil
}

// ProtectedRPC returns the first RPC that needs a token, or nil.
func (a *AuthModel) ProtectedRPC() *RuleModel {
	for _, r := range a.Rules {
		if r.Kind == spec.AccessAuthenticated || r.Kind == spec.AccessRoles {
			return r
		}
	}
	return nil
}

func newAuth(s *spec.Spec, fullService string) *AuthModel {
	if s.Auth == nil {
		return nil
	}
	am := &AuthModel{
		RolesClaim:   s.Auth.RolesClaim,
		MTLSIdentity: s.Auth.MTLSIdentity,
		DefaultKind:  s.Auth.Default,
		Default:      ruleExpr(s.AuthDefault()),
		Roles:        s.Roles(),
	}
	if j := s.Auth.JWT; j != nil {
		am.JWT, am.Issuer, am.Audience, am.JWKSURL = true, j.Issuer, j.Audience, j.JWKSURL
	}
	add := func(rpc string, a spec.Access, explicit, custom bool) {
		am.Rules = append(am.Rules, &RuleModel{RPC: rpc, Method: "/" + fullService + "/" + rpc, Expr: ruleExpr(a),
			Kind: a.Kind, Roles: a.Roles, Explicit: explicit, Custom: custom})
	}
	for _, e := range s.Entities {
		for _, op := range spec.AllOps {
			if !e.HasOp(op) {
				continue
			}
			a, explicit := s.OpAccess(e, op)
			add(rpcName(e, op), a, explicit, false)
		}
	}
	for _, m := range s.Methods {
		a, explicit := s.MethodAccess(m)
		add(m.Name, a, explicit, true)
	}
	for _, r := range am.Rules {
		am.Cases = append(am.Cases, authCases(r, am.Roles, am.JWT)...)
	}
	return am
}

func rpcName(e spec.Entity, op string) string {
	if op == "list" {
		return "List" + e.Plural
	}
	return strings.ToUpper(op[:1]) + op[1:] + e.Name
}

func ruleExpr(a spec.Access) string {
	switch a.Kind {
	case spec.AccessPublic:
		return "Public"
	case spec.AccessDeny:
		return "Deny"
	case spec.AccessRoles:
		return "Roles(" + quoteList(a.Roles) + ")"
	}
	return "Authenticated"
}

func quoteList(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = strconv.Quote(s)
	}
	return strings.Join(q, ", ")
}

func rolesExpr(roles []string) string {
	if len(roles) == 0 {
		return "nil"
	}
	return "[]string{" + quoteList(roles) + "}"
}

// authCases derives the expected answer for each kind of caller from the
// rule, independently of the generated interceptor, so the test checks the
// interceptor against the spec rather than against itself.
// Without JWT the only credential is a client certificate, which carries
// no roles.
func authCases(r *RuleModel, all []string, jwt bool) []AuthCase {
	want := func(token bool, roles []string) string {
		switch {
		case r.Kind == spec.AccessDeny:
			return "codes.PermissionDenied"
		case r.Kind == spec.AccessPublic:
			return "codes.OK"
		case !token:
			return "codes.Unauthenticated"
		case r.Kind == spec.AccessAuthenticated:
			return "codes.OK"
		}
		for _, have := range roles {
			for _, need := range r.Roles {
				if have == need {
					return "codes.OK"
				}
			}
		}
		return "codes.PermissionDenied"
	}
	cases := []AuthCase{
		{Method: r.Method, Caller: "anonymous", Code: want(false, nil), Roles: "nil"},
		{Method: r.Method, Caller: "no roles", Token: true, Code: want(true, nil), Roles: "nil"},
	}
	if !jwt {
		return cases
	}
	cases = append(cases, AuthCase{Method: r.Method, Caller: "all roles", Token: true, Code: want(true, all), Roles: rolesExpr(all)})
	if len(all) > 1 {
		for _, role := range all {
			cases = append(cases, AuthCase{Method: r.Method, Caller: "role " + role, Token: true,
				Code: want(true, []string{role}), Roles: rolesExpr([]string{role})})
		}
	}
	return cases
}

// AuthJWT reports whether the service verifies bearer tokens.
func (m *Model) AuthJWT() bool { return m.Auth != nil && m.Auth.JWT }

// AuthRequires lists the pinned module requirements of the auth package.
func (m *Model) AuthRequires() []string {
	if !m.AuthJWT() {
		return nil
	}
	return []string{KeyfuncModule + " " + KeyfuncVer, JWTModule + " " + JWTVersion}
}
