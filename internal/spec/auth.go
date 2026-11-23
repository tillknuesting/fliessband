package spec

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Auth switches on authentication and authorisation in the generated
// service. Without it the service accepts every caller, as before.
//
//	auth:
//	  jwt: {issuer: https://idp.example.com, audience: inventory, jwks_url: https://idp.example.com/.well-known/jwks.json}
//	  mtls_identity: true    # a verified client certificate also identifies the caller
//	  roles_claim: roles     # claim holding the caller's roles; dots reach into objects
//	  default: authenticated # rule for RPCs without one: authenticated, public or deny
//
// Entities name a rule per operation (access: {create: [admin], list: public})
// and methods one rule (access: [admin]). A rule is public, authenticated,
// deny, or a list of roles of which the caller needs at least one.
type Auth struct {
	JWT          *JWT   `yaml:"jwt" json:"jwt,omitempty"`
	MTLSIdentity bool   `yaml:"mtls_identity" json:"mtls_identity,omitempty"`
	RolesClaim   string `yaml:"roles_claim" json:"roles_claim,omitempty"`
	Default      string `yaml:"default" json:"default,omitempty"`
}

// JWT configures bearer tokens: signed JWTs (RS256 or ES256) whose keys come
// from the identity provider's JWKS endpoint.
type JWT struct {
	Issuer   string `yaml:"issuer" json:"issuer"`
	Audience string `yaml:"audience" json:"audience"`
	JWKSURL  string `yaml:"jwks_url" json:"jwks_url"`
}

// Access rule kinds.
const (
	AccessPublic        = "public"        // anyone, with or without a token
	AccessAuthenticated = "authenticated" // any caller with a valid identity
	AccessDeny          = "deny"          // nobody
	AccessRoles         = "roles"         // callers holding at least one of Roles
)

// DefaultRolesClaim is the claim roles are read from unless the spec says otherwise.
const DefaultRolesClaim = "roles"

// Access is the rule for one RPC. In YAML it is written as public,
// authenticated or deny, or as a list of roles.
type Access struct {
	Kind  string
	Roles []string
}

// UnmarshalYAML reads a rule word or a list of roles.
func (a *Access) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		a.Kind, a.Roles = n.Value, nil
		return nil
	case yaml.SequenceNode:
		var roles []string
		if err := n.Decode(&roles); err != nil {
			return fmt.Errorf("line %d: roles must be a list of names: %w", n.Line, err)
		}
		a.Kind, a.Roles = AccessRoles, roles
		if a.Roles == nil {
			a.Roles = []string{}
		}
		return nil
	}
	return fmt.Errorf("line %d: an access rule is public, authenticated, deny or a list of roles", n.Line)
}

// MarshalJSON writes the rule the way it is written in YAML.
func (a Access) MarshalJSON() ([]byte, error) {
	if a.Kind == AccessRoles {
		return json.Marshal(append([]string{}, a.Roles...))
	}
	return json.Marshal(a.Kind)
}

// UnmarshalJSON reads a rule word or a list of roles.
func (a *Access) UnmarshalJSON(b []byte) error {
	var word string
	if err := json.Unmarshal(b, &word); err == nil {
		a.Kind, a.Roles = word, nil
		return nil
	}
	var roles []string
	if err := json.Unmarshal(b, &roles); err != nil {
		return errors.New("an access rule is public, authenticated, deny or a list of roles")
	}
	a.Kind, a.Roles = AccessRoles, append([]string{}, roles...)
	return nil
}

// String describes the rule, e.g. "roles admin, reader".
func (a Access) String() string {
	if a.Kind == AccessRoles {
		return "roles " + strings.Join(a.Roles, ", ")
	}
	return a.Kind
}

// AuthDefault returns the rule for RPCs without one of their own.
func (s *Spec) AuthDefault() Access {
	if s.Auth == nil {
		return Access{Kind: AccessPublic}
	}
	return Access{Kind: s.Auth.Default}
}

// OpAccess returns the rule for an entity operation and whether the spec
// names it explicitly (otherwise the default applies).
func (s *Spec) OpAccess(e Entity, op string) (Access, bool) {
	if a, ok := e.Access[op]; ok && s.Auth != nil {
		return a, true
	}
	return s.AuthDefault(), false
}

// MethodAccess returns the rule for a custom method and whether the spec
// names it explicitly.
func (s *Spec) MethodAccess(m Method) (Access, bool) {
	if m.Access != nil && s.Auth != nil {
		return *m.Access, true
	}
	return s.AuthDefault(), false
}

// Roles returns every role the access rules name, sorted.
func (s *Spec) Roles() []string {
	var out []string
	add := func(a Access) {
		for _, r := range a.Roles {
			if !slices.Contains(out, r) {
				out = append(out, r)
			}
		}
	}
	for _, e := range s.Entities {
		for _, a := range e.Access {
			add(a)
		}
	}
	for _, m := range s.Methods {
		if m.Access != nil {
			add(*m.Access)
		}
	}
	slices.Sort(out)
	return out
}

func (s *Spec) applyAuthDefaults() {
	if s.Auth == nil {
		return
	}
	if s.Auth.RolesClaim == "" {
		s.Auth.RolesClaim = DefaultRolesClaim
	}
	if s.Auth.Default == "" {
		s.Auth.Default = AccessAuthenticated
	}
}

var (
	reRole       = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]*$`)
	reRolesClaim = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_:/-]*(\.[A-Za-z_][A-Za-z0-9_:/-]*)*$`)
	ruleWords    = []string{AccessPublic, AccessAuthenticated, AccessDeny}
)

// validateAuth checks the auth block and every access rule and example
// caller. Rules without an auth block are an error, so that a spec cannot
// look protected while the service is open.
func (s *Spec) validateAuth() []Problem {
	var ps []Problem
	add := func(path, msg string) { ps = append(ps, Problem{Path: path, Message: msg}) }
	warn := func(path, msg string) { ps = append(ps, Problem{Path: path, Message: msg, Warning: true}) }

	if a := s.Auth; a != nil {
		if a.JWT == nil && !a.MTLSIdentity {
			add("auth", "configure jwt, mtls_identity or both; otherwise no caller can be identified")
		}
		if j := a.JWT; j != nil {
			if strings.TrimSpace(j.Issuer) == "" {
				add("auth.jwt.issuer", "the token issuer (iss claim) is required, e.g. \"https://idp.example.com\"")
			}
			if strings.TrimSpace(j.Audience) == "" {
				add("auth.jwt.audience", "the audience (aud claim) tokens must carry is required, e.g. the service name")
			}
			switch u, err := url.Parse(j.JWKSURL); {
			case j.JWKSURL == "":
				add("auth.jwt.jwks_url", "the URL of the identity provider's JSON Web Key Set is required")
			case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http"):
				add("auth.jwt.jwks_url", "must be an absolute https URL, e.g. \"https://idp.example.com/.well-known/jwks.json\"")
			case u.Scheme == "http" && !isLocalHost(u.Hostname()):
				add("auth.jwt.jwks_url", "use https: keys fetched over plain http can be swapped by anyone on the path")
			}
		}
		if !reRolesClaim.MatchString(a.RolesClaim) {
			add("auth.roles_claim", "a claim name such as \"roles\"; use dots for nested claims, e.g. \"realm_access.roles\"")
		}
		switch a.Default {
		case AccessAuthenticated, AccessDeny:
		case AccessPublic:
			warn("auth.default", "every RPC without its own rule is open to anonymous callers; prefer authenticated and mark public RPCs explicitly")
		default:
			add("auth.default", fmt.Sprintf("unknown default %q; use authenticated, public or deny", a.Default))
		}
	}

	roles := s.Roles()
	if s.Auth != nil && s.Auth.JWT == nil && len(roles) > 0 {
		add("auth.jwt", "role rules need jwt: client certificate identities carry no roles")
	}
	for i, e := range s.Entities {
		p := fmt.Sprintf("entities[%d].access", i)
		if len(e.Access) > 0 && s.Auth == nil {
			add(p, "access rules need an auth: block that says how callers are identified")
			continue
		}
		for _, op := range sortedKeys(e.Access) {
			if !e.HasOp(op) {
				add(p+"."+op, fmt.Sprintf("the entity has no %q operation; it has %s", op, strings.Join(e.Ops, ", ")))
			}
			ps = append(ps, validateAccess(p+"."+op, e.Access[op])...)
		}
	}
	for i, m := range s.Methods {
		p := fmt.Sprintf("methods[%d]", i)
		if m.Access != nil {
			if s.Auth == nil {
				add(p+".access", "access rules need an auth: block that says how callers are identified")
			} else {
				ps = append(ps, validateAccess(p+".access", *m.Access)...)
			}
		}
		for j, ex := range m.Examples {
			if ex.As == nil {
				continue
			}
			ep := fmt.Sprintf("%s.examples[%d].as", p, j)
			if s.Auth == nil || s.Auth.JWT == nil {
				add(ep, "an example caller needs auth.jwt: the acceptance tests send it as a token")
				continue
			}
			for _, r := range ex.As {
				switch {
				case !reRole.MatchString(r):
					add(ep, fmt.Sprintf("role %q: use letters, digits and _ . : -", r))
				case !slices.Contains(roles, r):
					warn(ep, fmt.Sprintf("role %q appears in no access rule", r))
				}
			}
		}
	}
	return ps
}

func validateAccess(p string, a Access) []Problem {
	var ps []Problem
	add := func(msg string) { ps = append(ps, Problem{Path: p, Message: msg}) }
	if a.Kind != AccessRoles {
		if !slices.Contains(ruleWords, a.Kind) {
			add(fmt.Sprintf("unknown rule %q; use public, authenticated, deny or a list of roles", a.Kind))
		}
		return ps
	}
	if len(a.Roles) == 0 {
		add("list at least one role; use authenticated for any signed-in caller")
	}
	seen := map[string]bool{}
	for _, r := range a.Roles {
		switch {
		case !reRole.MatchString(r):
			add(fmt.Sprintf("role %q: use letters, digits and _ . : -", r))
		case slices.Contains(ruleWords, r):
			add(fmt.Sprintf("%q is a rule, not a role; write it without brackets", r))
		case seen[r]:
			add(fmt.Sprintf("duplicate role %q", r))
		}
		seen[r] = true
	}
	return ps
}

func isLocalHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}
