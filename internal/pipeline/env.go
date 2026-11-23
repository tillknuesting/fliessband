package pipeline

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Child processes run generated code, part of which an LLM wrote, so they
// get an allowlisted environment: nothing that could carry a credential
// (API keys, cloud and VCS tokens, the LLM client's settings) reaches them.
var envAllow = []string{
	"HOME", "USER", "LOGNAME", "TMPDIR", "LANG", "LC_ALL", "TZ",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
	"GOPATH", "GOCACHE", "GOMODCACHE", "GOPROXY", "GOSUMDB", "GONOSUMDB", "GONOSUMCHECK",
	"GOPRIVATE", "GONOPROXY", "GOINSECURE", "GOVCS", "GOEXPERIMENT", "CGO_ENABLED",
	"BUF_CACHE_DIR", "XDG_CACHE_HOME", "GOLANGCI_LINT_CACHE",
}

// envNetwork is only passed to stages that download modules.
var envNetwork = []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"}

// goEnv resolves the Go cache locations once, so they stay the same even
// though the child environment is minimal.
func goEnv() map[string]string {
	out := map[string]string{}
	b, err := exec.Command("go", "env", "-json", "GOPATH", "GOCACHE", "GOMODCACHE").Output()
	if err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

// childEnv builds the environment for a tool. network allows module
// downloads; without it Go runs offline against the module cache, so
// generated code cannot fetch anything new after the dependency stage.
func (m *Manager) childEnv(network bool, extra ...string) []string {
	var env []string
	allow := envAllow
	if network {
		allow = append(slices.Clone(allow), envNetwork...)
	}
	for _, k := range allow {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range m.goPaths {
		if v != "" && os.Getenv(k) == "" {
			env = append(env, k+"="+v)
		}
	}
	path := os.Getenv("PATH")
	if goroot, err := exec.LookPath("go"); err == nil {
		path = filepath.Dir(goroot) + string(os.PathListSeparator) + path
	}
	if m.toolsBin != "" {
		path = m.toolsBin + string(os.PathListSeparator) + path
	}
	env = append(env, "PATH="+path, "GOTOOLCHAIN=local", "GOWORK=off")
	if !network {
		env = append(env, "GOPROXY=off", "GOFLAGS=-mod=readonly")
	}
	return append(env, extra...)
}

// isSecretName reports whether an environment variable name looks like it
// holds a credential. Tests use it to check childEnv.
func isSecretName(k string) bool {
	k = strings.ToUpper(k)
	for _, s := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "AUTH"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return strings.HasPrefix(k, "FLIESSBAND_LLM") || strings.HasPrefix(k, "OPENAI") || strings.HasPrefix(k, "AWS_")
}
