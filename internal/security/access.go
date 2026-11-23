package security

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/tillknuesting/fliessband/internal/metrics"
	"github.com/tillknuesting/fliessband/internal/spec"
)

const accessArea = "Access control"

// access audits authentication and authorisation: whether the service
// identifies callers at all, whether every RPC has a deliberate rule,
// whether token verification checks what it must, and whether tokens stay
// out of the logs.
func (a *audit) access(s *spec.Spec) {
	authFile, rulesFile := "internal/auth/auth.go", "internal/auth/rules.go"
	c := metrics.SecurityCheck{ID: "authn", Area: accessArea, Title: "Callers are authenticated", File: "spec.yaml"}
	if s.Auth == nil {
		c.Status = "warn"
		c.Detail = "The service has no auth: block, so every RPC answers any caller that can reach the port. mTLS " +
			"(TLS_CLIENT_CA_FILE) only proves a client holds a certificate from the CA; nothing decides who may call what. " +
			"Add auth: to the spec, or enforce access in a gateway or mesh in front of the service."
		a.add(c)
		for _, id := range []struct{ id, title string }{{"authz-wired", "Access rules enforced on every call"},
			{"rpc-rules", "Every RPC has a deliberate rule"}, {"jwt-verify", "Tokens fully verified"}, {"token-logs", "Tokens kept out of logs"}} {
			a.add(metrics.SecurityCheck{ID: id.id, Area: accessArea, Title: id.title, Status: "skip", Detail: "The service has no auth: block."})
		}
		return
	}
	var how []string
	if s.Auth.JWT != nil {
		how = append(how, fmt.Sprintf("bearer JWTs from %s for audience %q", s.Auth.JWT.Issuer, s.Auth.JWT.Audience))
	}
	if s.Auth.MTLSIdentity {
		how = append(how, "verified client certificates")
	}
	c.File, c.Line = authFile, a.find(authFile, `^func \(a \*Authorizer\) authenticate\(`)
	if c.Line > 0 {
		c.Status, c.Detail = "pass", "Callers are identified by "+strings.Join(how, " and ")+"; invalid credentials are rejected, never treated as anonymous."
	} else {
		c.Status, c.Detail = "fail", "The spec asks for auth, but the generated auth package has no authenticate function."
	}
	a.add(c)

	a.has(accessArea, "authz-wired", "Access rules enforced on every call", "internal/server/server.go", `ChainUnaryInterceptor\(.*o\.Auth\.Unary\(\)`,
		"The auth interceptor runs on every unary call (and on streams) before the handler; health and reflection are the documented exceptions.",
		"The server does not install the auth interceptor, so the access rules are never checked.", "fail")

	a.rpcRules(s, rulesFile)
	a.jwtVerify(s, authFile)
	a.tokenLogs()
}

// rpcRules fails when a custom method is public only because the default
// is, since custom methods are where an LLM writes business logic that may
// expose data; implicit public CRUD operations are a warning.
func (a *audit) rpcRules(s *spec.Spec, rulesFile string) {
	c := metrics.SecurityCheck{ID: "rpc-rules", Area: accessArea, Title: "Every RPC has a deliberate rule", File: rulesFile, Line: a.find(rulesFile, `^var Rules = `)}
	var implicitCustom, implicitOps, missing []string
	explicit, total := 0, 0
	check := func(rpc string, acc spec.Access, own bool, custom bool) {
		total++
		if own {
			explicit++
		}
		if !own && acc.Kind == spec.AccessPublic {
			if custom {
				implicitCustom = append(implicitCustom, rpc)
			} else {
				implicitOps = append(implicitOps, rpc)
			}
		}
		if a.find(rulesFile, regexp.QuoteMeta(`/`+rpc+`":`)) == 0 {
			missing = append(missing, rpc)
		}
	}
	for _, e := range s.Entities {
		names := e.RPCNames() // in the order of opsOf
		for i, op := range opsOf(e) {
			acc, own := s.OpAccess(e, op)
			check(names[i], acc, own, false)
		}
	}
	for _, m := range s.Methods {
		acc, own := s.MethodAccess(m)
		check(m.Name, acc, own, true)
	}
	switch {
	case len(missing) > 0:
		c.Status, c.Detail = "fail", fmt.Sprintf("The generated rule table lacks %s; those calls fall back to the default rule.", strings.Join(missing, ", "))
	case len(implicitCustom) > 0:
		c.Status = "fail"
		c.Detail = fmt.Sprintf("Custom methods %s are public only because auth.default is public. Give them access: public if that is meant, or a stricter rule.", strings.Join(implicitCustom, ", "))
	case len(implicitOps) > 0:
		c.Status = "warn"
		c.Detail = fmt.Sprintf("%s are public only because auth.default is public. Prefer default: authenticated and mark public operations explicitly.", strings.Join(implicitOps, ", "))
	default:
		c.Status = "pass"
		c.Detail = fmt.Sprintf("%d of %d RPCs have their own rule; the rest fall to the default, %s, so nothing is public by accident.", explicit, total, s.Auth.Default)
	}
	a.add(c)
}

// opsOf lists the entity's operations in the order RPCNames returns them.
func opsOf(e spec.Entity) []string {
	var out []string
	for _, op := range spec.AllOps {
		if e.HasOp(op) {
			out = append(out, op)
		}
	}
	return out
}

func (a *audit) jwtVerify(s *spec.Spec, authFile string) {
	c := metrics.SecurityCheck{ID: "jwt-verify", Area: accessArea, Title: "Tokens fully verified", File: authFile}
	if s.Auth.JWT == nil {
		c.Status, c.Detail = "skip", "The service accepts no bearer tokens, only client certificates."
		a.add(c)
		return
	}
	var lacks []string
	for _, chk := range []struct{ re, what string }{
		{`jwt\.WithValidMethods\(\[\]string\{"RS256", "ES256"\}\)`, "an allowlist of asymmetric algorithms"},
		{`jwt\.WithIssuer\(`, "the issuer"},
		{`jwt\.WithAudience\(`, "the audience"},
		{`jwt\.WithExpirationRequired\(`, "a required expiry"},
	} {
		if a.find(authFile, chk.re) == 0 {
			lacks = append(lacks, chk.what)
		}
	}
	if len(lacks) == 0 {
		c.Status, c.Line = "pass", a.find(authFile, `jwt\.WithValidMethods\(`)
		c.Detail = "Tokens must be signed with RS256 or ES256 by a key from the JWKS, name the issuer and audience, and carry an expiry; exp and nbf are checked with a small leeway."
	} else {
		c.Status = "fail"
		c.Detail = "Token verification does not check " + strings.Join(lacks, ", ") + "."
	}
	a.add(c)
}

var (
	anyLogCall  = regexp.MustCompile(`\b(?:log|slog|logger)\.\w+\(`)
	stringLit   = regexp.MustCompile("\"(?:[^\"\\\\]|\\\\.)*\"|`[^`]*`")
	credentials = regexp.MustCompile(`(?i)\b(?:raw|tok|token|tokens|bearer|authorization|md|claims|vals)\b`)
)

// tokenLogs fails when a log call in the auth, server or service code
// passes something that holds a token or the request metadata.
func (a *audit) tokenLogs() {
	c := metrics.SecurityCheck{ID: "token-logs", Area: accessArea, Title: "Tokens kept out of logs"}
	paths := make([]string, 0, len(a.files))
	for p := range a.files {
		if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") &&
			(strings.HasPrefix(p, "internal/auth/") || strings.HasPrefix(p, "internal/server/") || strings.HasPrefix(p, "internal/service/")) {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	for _, p := range paths {
		if line := logsCredentials(a.files[p]); line > 0 {
			c.Status, c.File, c.Line = "fail", p, line
			c.Detail = "A log call passes a token, the authorization header or request metadata; anyone who can read the logs could replay it."
			a.add(c)
			return
		}
	}
	c.Status = "pass"
	c.Detail = fmt.Sprintf("No log call in %d auth, server and service files passes a token, the authorization header or request metadata.", len(paths))
	a.add(c)
}

// logsCredentials returns the line of the first log call whose arguments,
// string literals aside, name a credential, or 0.
func logsCredentials(src string) int {
	for _, m := range anyLogCall.FindAllStringIndex(src, -1) {
		depth, end := 1, m[1]
		for end < len(src) && depth > 0 {
			switch src[end] {
			case '(':
				depth++
			case ')':
				depth--
			}
			end++
		}
		if credentials.MatchString(stringLit.ReplaceAllString(src[m[1]:end], `""`)) {
			return strings.Count(src[:m[0]], "\n") + 1
		}
	}
	return 0
}
