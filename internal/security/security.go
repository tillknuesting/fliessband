// Package security audits a generated service. Every check reads the files
// the line produced (or a station's result) and records where it looked, so
// a reader can verify each claim rather than trust a template's intent.
package security

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/tillknuesting/fliessband/internal/metrics"
	"github.com/tillknuesting/fliessband/internal/spec"
)

// Ran reports whether a station of the line ran (passed, warned or failed).
type Ran func(station string) bool

type audit struct {
	root  string
	files map[string]string
	out   []metrics.SecurityCheck
}

// Audit runs every check against the service in root.
func Audit(root string, s *spec.Spec, rep *metrics.Report, ran Ran) *metrics.Security {
	a := &audit{root: root, files: map[string]string{}}
	a.load()
	a.transport(s)
	a.limits()
	a.input(s)
	a.logging()
	a.access(s)
	a.container(s)
	a.kubernetes(s)
	a.ci(s)
	a.code(rep)
	a.dependencies(rep, ran)
	a.supply(rep, ran)
	sec := &metrics.Security{Checks: a.out}
	for _, c := range a.out {
		switch c.Status {
		case "pass":
			sec.Passed++
		case "warn":
			sec.Warned++
		case "fail":
			sec.Failed++
		}
	}
	return sec
}

// load reads the service's text files, leaving out generated protobuf code
// and checksums.
func (a *audit) load() {
	_ = filepath.WalkDir(a.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable entry is skipped, not fatal
		}
		rel, _ := filepath.Rel(a.root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == "gen" || rel == "bin" || strings.HasPrefix(d.Name(), ".") && rel != ".github" && rel != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if rel == "go.sum" {
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > 1<<20 {
			return nil //nolint:nilerr // large or unreadable files are not source
		}
		b, err := os.ReadFile(p) //nolint:gosec // walking the run's own service directory
		if err == nil {
			a.files[rel] = string(b)
		}
		return nil
	})
}

func (a *audit) add(c metrics.SecurityCheck) { a.out = append(a.out, c) }

// find returns the first line in file matching re, or 0.
func (a *audit) find(file, re string) int {
	src, ok := a.files[file]
	if !ok {
		return 0
	}
	rx := regexp.MustCompile(re)
	for i, l := range strings.Split(src, "\n") {
		if rx.MatchString(l) {
			return i + 1
		}
	}
	return 0
}

// has adds a check that passes when file contains re.
func (a *audit) has(area, id, title, file, re, pass, fail string, failStatus string) {
	c := metrics.SecurityCheck{ID: id, Area: area, Title: title, File: file}
	if _, ok := a.files[file]; !ok {
		c.Status, c.Detail = "skip", file+" was not generated"
		a.add(c)
		return
	}
	if n := a.find(file, re); n > 0 {
		c.Status, c.Detail, c.Line = "pass", pass, n
	} else {
		c.Status, c.Detail = failStatus, fail
	}
	a.add(c)
}

func (a *audit) transport(s *spec.Spec) {
	const area = "Transport"
	a.has(area, "tls13", "TLS 1.3 only", "internal/server/tls.go", `MinVersion:\s*tls\.VersionTLS13`,
		"TLS connections must negotiate TLS 1.3.", "The TLS config does not require TLS 1.3.", "fail")
	a.has(area, "mtls", "Client certificates (mTLS) available", "internal/server/tls.go", `RequireAndVerifyClientCert`,
		"Setting TLS_CLIENT_CA_FILE makes the server require and verify client certificates.", "The server cannot require client certificates.", "warn")
	c := metrics.SecurityCheck{ID: "tls-default", Area: area, Title: "TLS on by default", File: "internal/config/config.go", Status: "warn",
		Detail: "The server serves plaintext gRPC until TLS_CERT_FILE and TLS_KEY_FILE are set. Set them, or terminate TLS in a service mesh in front of it."}
	c.Line = a.find(c.File, `TLS_CERT_FILE`)
	a.add(c)
	r := metrics.SecurityCheck{ID: "reflection", Area: area, Title: "Server reflection off", File: "internal/server/server.go"}
	if s.Features.HasReflection() {
		r.Status, r.Line = "warn", a.find(r.File, `reflection\.Register`)
		r.Detail = "Reflection lists every RPC and message to any client that connects. Useful in development; set features.reflection: false for production."
	} else {
		r.Status, r.Detail = "pass", "Reflection is not registered, so clients cannot list the API."
	}
	a.add(r)
}

func (a *audit) limits() {
	const area = "Abuse limits"
	f := "internal/server/server.go"
	a.has(area, "max-msg", "Request size capped", f, `MaxRecvMsgSize\(`, "Requests larger than the cap are rejected before they are decoded.", "No request size cap is set; gRPC's default applies.", "warn")
	a.has(area, "streams", "Concurrent streams capped", f, `MaxConcurrentStreams\(`, "Each connection is limited in how many calls it can open at once.", "Streams per connection are unlimited.", "warn")
	a.has(area, "shed", "Load shedding", f, `Limit\(`, "Calls beyond MAX_IN_FLIGHT fail fast with RESOURCE_EXHAUSTED instead of queueing.", "Nothing limits calls in flight.", "warn")
	a.has(area, "deadline", "Default deadline", f, `Deadline\(`, "Calls without a deadline get a default one, so a slow client cannot hold a handler forever.", "Calls without a deadline can run forever.", "warn")
	a.has(area, "keepalive", "Keepalive enforcement", f, `KeepaliveEnforcementPolicy`, "Clients that ping too often are disconnected.", "Clients may ping as often as they like.", "warn")
	a.has(area, "recovery", "Panics contained", f, `Recovery\(`, "A panic in a handler returns INTERNAL for that call instead of crashing the server.", "A panic in a handler crashes the server.", "fail")
}

// input flags string fields a client can fill without bound.
func (a *audit) input(s *spec.Spec) {
	var open []string
	check := func(owner string, fs []spec.Field) {
		for _, f := range fs {
			if f.Type == "string" && f.MaxLen == nil && f.Pattern == "" {
				open = append(open, owner+"."+f.Name)
			}
		}
	}
	for _, e := range s.Entities {
		check(e.Name, e.Fields)
	}
	for _, m := range s.Methods {
		check(m.Name, m.Request)
	}
	c := metrics.SecurityCheck{ID: "bounded-strings", Area: "Input validation", Title: "String inputs bounded", File: "spec.yaml"}
	if len(open) == 0 {
		c.Status, c.Detail = "pass", "Every string field has max_len or a pattern, enforced before the store is called."
	} else {
		c.Status = "warn"
		shown := open
		if len(shown) > 6 {
			shown = append(slices.Clone(shown[:6]), fmt.Sprintf("and %d more", len(open)-6))
		}
		c.Detail = fmt.Sprintf("%d string fields accept any length up to the request size cap: %s. Add max_len or a pattern in the spec.", len(open), strings.Join(shown, ", "))
	}
	a.add(c)
}

func (a *audit) logging() {
	f := "internal/server/interceptors.go"
	c := metrics.SecurityCheck{ID: "log-payloads", Area: "Logging", Title: "Request bodies kept out of logs", File: f}
	src, ok := a.files[f]
	i := strings.Index(src, "func Logging(")
	if !ok || i < 0 {
		c.Status, c.Detail = "skip", "No logging interceptor was generated."
		a.add(c)
		return
	}
	body := src[i:]
	if j := strings.Index(body[1:], "\nfunc "); j > 0 {
		body = body[:j+1]
	}
	c.Line = strings.Count(src[:i], "\n") + 1
	if logsBodies(body) {
		c.Status, c.Detail = "fail", "The logging interceptor writes request or response bodies, which can put personal data in logs."
	} else {
		c.Status, c.Detail = "pass", "Calls are logged by method, status code and duration only; bodies never reach the logs."
	}
	a.add(c)
}

var (
	logCall = regexp.MustCompile(`\blog\.\w+\(`)
	bodyArg = regexp.MustCompile(`\b(req|resp)\b`)
)

// logsBodies reports whether any log call in src passes req or resp, reading
// each call's arguments up to its matching parenthesis.
func logsBodies(src string) bool {
	for _, m := range logCall.FindAllStringIndex(src, -1) {
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
		if bodyArg.MatchString(src[m[1]:end]) {
			return true
		}
	}
	return false
}

func (a *audit) container(s *spec.Spec) {
	const area = "Container"
	if !s.Features.HasDocker() {
		a.add(metrics.SecurityCheck{ID: "container", Area: area, Title: "Container image", Status: "skip", Detail: "features.docker is off."})
		return
	}
	f := "Dockerfile"
	a.has(area, "distroless", "Minimal base image", f, `^FROM\s+gcr\.io/distroless/`, "The runtime image is distroless: no shell or package manager to exploit.", "The runtime image is not distroless.", "warn")
	a.has(area, "nonroot", "Runs as non-root", f, `^USER\s+nonroot`, "The process runs as an unprivileged user.", "The container runs as root.", "fail")
	a.has(area, "static", "Static, stripped build", f, `CGO_ENABLED=0.*-trimpath`, "The binary is static and built without local paths.", "The binary links C libraries or embeds build paths.", "warn")
	c := metrics.SecurityCheck{ID: "digest", Area: area, Title: "Base images pinned by digest", File: f}
	from := regexp.MustCompile(`(?m)^FROM\s+(\S+)`).FindAllStringSubmatch(a.files[f], -1)
	var tags []string
	for _, m := range from {
		if !strings.Contains(m[1], "@sha256:") {
			tags = append(tags, m[1])
		}
	}
	if len(tags) == 0 {
		c.Status, c.Detail = "pass", "Every base image is pinned by digest."
	} else {
		c.Status, c.Line = "warn", a.find(f, `^FROM`)
		c.Detail = fmt.Sprintf("%s pulled by tag, so a rebuild can get a different image. Pin by digest and let Renovate or Dependabot update it.", strings.Join(tags, " and "))
	}
	a.add(c)
}

func (a *audit) kubernetes(s *spec.Spec) {
	const area = "Kubernetes"
	if !s.Features.HasKubernetes() {
		a.add(metrics.SecurityCheck{ID: "k8s", Area: area, Title: "Kubernetes manifests", Status: "skip", Detail: "features.kubernetes is off."})
		return
	}
	f := "deploy/k8s/deployment.yaml"
	a.has(area, "k8s-nonroot", "Pod runs as non-root", f, `runAsNonRoot:\s*true`, "Kubernetes refuses to start the pod as root.", "runAsNonRoot is not set.", "fail")
	a.has(area, "k8s-rofs", "Read-only root filesystem", f, `readOnlyRootFilesystem:\s*true`, "The container cannot write to its own filesystem.", "The root filesystem is writable.", "warn")
	a.has(area, "k8s-escalation", "No privilege escalation", f, `allowPrivilegeEscalation:\s*false`, "setuid binaries cannot gain privileges.", "allowPrivilegeEscalation is not false.", "fail")
	a.has(area, "k8s-caps", "All capabilities dropped", f, `drop:\s*\[?\s*"?ALL`, "The container keeps no Linux capabilities.", "Linux capabilities are not dropped.", "warn")
	a.has(area, "k8s-seccomp", "Seccomp profile", f, `seccompProfile:`, "System calls are filtered by the runtime's default seccomp profile.", "No seccomp profile is set.", "warn")
	a.has(area, "k8s-token", "No service account token", f, `automountServiceAccountToken:\s*false`, "The pod gets no Kubernetes API token it does not need.", "A service account token is mounted into the pod.", "warn")
	a.has(area, "k8s-limits", "Resource limits", f, `limits:`, "CPU and memory limits stop one pod starving its node.", "No resource limits are set.", "warn")
}

func (a *audit) ci(s *spec.Spec) {
	const area = "CI"
	f := ".github/workflows/ci.yml"
	if !s.Features.HasCI() {
		a.add(metrics.SecurityCheck{ID: "ci", Area: area, Title: "CI workflow", Status: "skip", Detail: "features.ci is off."})
		return
	}
	a.has(area, "ci-permissions", "Least-privilege token", f, `^permissions:`, "The workflow's GITHUB_TOKEN gets only the permissions listed.", "The workflow token has the repository's default permissions.", "warn")
	c := metrics.SecurityCheck{ID: "ci-pinned", Area: area, Title: "Actions pinned by commit", File: f}
	uses := regexp.MustCompile(`uses:\s*(\S+)@(\S+)`).FindAllStringSubmatch(a.files[f], -1)
	var loose []string
	for _, u := range uses {
		if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(u[2]) {
			loose = append(loose, u[1]+"@"+u[2])
		}
	}
	if len(loose) == 0 {
		c.Status, c.Detail = "pass", "Every action is pinned to a commit SHA."
	} else {
		c.Status, c.Line = "warn", a.find(f, `uses:`)
		c.Detail = fmt.Sprintf("%d of %d actions use a movable tag (%s); a compromised tag would run in your CI. Pin them to commit SHAs.", len(loose), len(uses), strings.Join(loose[:min(3, len(loose))], ", "))
	}
	a.add(c)
}

var secrets = []struct{ name, re string }{
	{"a private key", `-----BEGIN [A-Z ]*PRIVATE KEY-----`},
	{"an AWS access key", `\bAKIA[0-9A-Z]{16}\b`},
	{"a GitHub token", `\bgh[pousr]_[A-Za-z0-9]{36}\b`},
	{"an API key", `\bsk-[A-Za-z0-9_-]{20,}`},
	{"a hard-coded password", `(?i)\b(password|passwd|secret|api_?key)\s*[:=]\s*"[^"$]{8,}"`},
}

func (a *audit) code(rep *metrics.Report) {
	const area = "Code"
	var gosec []metrics.Finding
	for _, f := range rep.Findings.Lint {
		if f.Code == "gosec" {
			gosec = append(gosec, f)
		}
	}
	c := metrics.SecurityCheck{ID: "gosec", Area: area, Title: "gosec clean"}
	if len(gosec) == 0 {
		c.Status, c.Detail = "pass", "gosec (through golangci-lint) found no issues: no SQL built from strings, weak crypto, unchecked file paths or similar."
	} else {
		c.Status, c.File, c.Line = "fail", gosec[0].File, gosec[0].Line
		c.Detail = fmt.Sprintf("gosec reports %d issues; the first: %s", len(gosec), gosec[0].Message)
	}
	a.add(c)

	s := metrics.SecurityCheck{ID: "secrets", Area: area, Title: "No secrets in the source"}
	paths := make([]string, 0, len(a.files))
	for p := range a.files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		for _, sp := range secrets {
			if n := a.find(p, sp.re); n > 0 {
				s.Status, s.File, s.Line, s.Detail = "fail", p, n, "Looks like "+sp.name+". Move it to configuration or a secret store."
				break
			}
		}
		if s.Status != "" {
			break
		}
	}
	if s.Status == "" {
		s.Status, s.Detail = "pass", fmt.Sprintf("No private keys, cloud or API keys, or hard-coded passwords in %d files.", len(paths))
	}
	a.add(s)

	pol := metrics.SecurityCheck{ID: "policy", Area: area, Title: "LLM code inside its policy"}
	switch {
	case len(rep.Findings.Policy) > 0:
		f := rep.Findings.Policy[0]
		pol.Status, pol.File, pol.Line = "fail", f.File, f.Line
		pol.Detail = fmt.Sprintf("%d policy violations in LLM-written code; the first: %s", len(rep.Findings.Policy), f.Message)
	case len(rep.Factory.RejectedWrites) > 0:
		pol.Status = "warn"
		pol.Detail = fmt.Sprintf("The model tried to write files it does not own (%s); the factory refused.", strings.Join(rep.Factory.RejectedWrites, ", "))
	default:
		pol.Status, pol.Detail = "pass", "LLM-written code uses only allowed imports, has no init, panic or compiler directives, and wrote no file outside its own two."
	}
	a.add(pol)
}

func (a *audit) dependencies(rep *metrics.Report, ran Ran) {
	const area = "Dependencies"
	v := metrics.SecurityCheck{ID: "govulncheck", Area: area, Title: "No known vulnerabilities reached"}
	switch {
	case !ran("vulncheck"):
		v.Status, v.Detail = "skip", "The vulnerabilities station did not run."
	case len(rep.Findings.Vulns) > 0:
		f := rep.Findings.Vulns[0]
		v.Status, v.File, v.Line = "fail", f.File, f.Line
		v.Detail = fmt.Sprintf("govulncheck finds %d vulnerabilities that the code can reach; the first: %s", len(rep.Findings.Vulns), f.Message)
	default:
		v.Status, v.Detail = "pass", "govulncheck finds no known vulnerability that the code can actually call, in the modules or the Go standard library."
	}
	a.add(v)
	fz := metrics.SecurityCheck{ID: "fuzz", Area: area, Title: "Fuzzing found no crashes"}
	switch {
	case rep.Fuzz == nil:
		fz.Status, fz.Detail = "skip", "The fuzz station did not run."
	case rep.Fuzz.Failures > 0:
		fz.Status, fz.Detail = "fail", fmt.Sprintf("%d fuzz targets found an input that crashes or breaks validation.", rep.Fuzz.Failures)
	default:
		fz.Status, fz.Detail = "pass", fmt.Sprintf("%d fuzz targets ran %d inputs through validation without a crash.", len(rep.Fuzz.Targets), rep.Fuzz.Execs)
	}
	a.add(fz)
}

func (a *audit) supply(rep *metrics.Report, ran Ran) {
	const area = "Supply chain"
	c := metrics.SecurityCheck{ID: "sbom", Area: area, Title: "SBOM and provenance"}
	switch {
	case rep.Supply != nil:
		c.Status = "pass"
		c.Detail = fmt.Sprintf("A CycloneDX SBOM lists %d components and a SLSA provenance statement pins the binary (sha256 %s…).", rep.Supply.Components, rep.Supply.BinarySHA256[:min(12, len(rep.Supply.BinarySHA256))])
	case !ran("sbom"):
		c.Status, c.Detail = "skip", "The supply chain station did not run."
	default:
		c.Status, c.Detail = "warn", "The supply chain station ran but produced no SBOM."
	}
	a.add(c)
}
