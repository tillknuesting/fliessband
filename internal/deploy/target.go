// Package deploy ships a run that passed the line to a Kubernetes cluster:
// it builds the service's image, gets it into the cluster, applies the
// generated manifests through a kustomize overlay, waits for the rollout,
// rolls back when the new version does not become ready, and checks the
// live service over gRPC.
//
// The factory deploys code that an LLM helped write, so it only deploys to
// clusters it was told about: local development clusters (k3d, kind,
// minikube, Docker Desktop, OrbStack, Colima, Rancher Desktop) are found on
// their own, and any other cluster has to be named in FLIESSBAND_DEPLOY_TARGETS.
package deploy

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Strategy is how a built image reaches the cluster's nodes.
type Strategy string

const (
	K3d      Strategy = "k3d"      // k3d image import
	Kind     Strategy = "kind"     // kind load docker-image
	Minikube Strategy = "minikube" // minikube image load
	Shared   Strategy = "shared"   // the cluster runs on the Docker engine that builds the image
	Registry Strategy = "registry" // docker push to a registry the cluster pulls from
)

// Target is a cluster and namespace the factory may deploy to.
type Target struct {
	Name      string   `json:"name"`
	Context   string   `json:"context"`
	Namespace string   `json:"namespace"`
	Strategy  Strategy `json:"strategy"`
	Cluster   string   `json:"cluster,omitempty"`  // k3d, kind or minikube cluster name
	Registry  string   `json:"registry,omitempty"` // e.g. ghcr.io/acme
	Local     bool     `json:"local"`
}

// DefaultNamespace is used when a target names none.
const DefaultNamespace = "fliessband"

var targetName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// detect works out how images reach a cluster from its context name.
func detect(context string) (s Strategy, cluster string, local bool) {
	switch {
	case strings.HasPrefix(context, "k3d-"):
		return K3d, strings.TrimPrefix(context, "k3d-"), true
	case strings.HasPrefix(context, "kind-"):
		return Kind, strings.TrimPrefix(context, "kind-"), true
	case context == "minikube":
		return Minikube, "minikube", true
	case context == "docker-desktop", context == "orbstack", context == "colima", context == "rancher-desktop":
		return Shared, "", true
	}
	return Registry, "", false
}

// ParseTargets reads targets written as name=context[/namespace][@registry],
// separated by commas, as in FLIESSBAND_DEPLOY_TARGETS. A cluster that is
// not a known local one needs a registry to push to.
func ParseTargets(s string) ([]Target, error) {
	var out []Target
	seen := map[string]bool{}
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, rest, ok := strings.Cut(part, "=")
		if !ok || !targetName.MatchString(name) {
			return nil, fmt.Errorf("deploy target %q: write name=context[/namespace][@registry], with a lowercase name", part)
		}
		if seen[name] {
			return nil, fmt.Errorf("deploy target %q is named twice", name)
		}
		seen[name] = true
		t := Target{Name: name, Namespace: DefaultNamespace}
		rest, t.Registry, _ = strings.Cut(rest, "@")
		t.Context, t.Namespace, _ = strings.Cut(rest, "/")
		if t.Namespace == "" {
			t.Namespace = DefaultNamespace
		}
		if t.Context == "" {
			return nil, fmt.Errorf("deploy target %q has no kube context", name)
		}
		if !dnsLabel.MatchString(t.Namespace) {
			return nil, fmt.Errorf("deploy target %q: %q is not a valid namespace", name, t.Namespace)
		}
		t.Strategy, t.Cluster, t.Local = detect(t.Context)
		if t.Registry != "" {
			t.Strategy = Registry
		} else if t.Strategy == Registry {
			return nil, fmt.Errorf("deploy target %q: %s is not a local cluster, so its nodes cannot see images built here; add @registry (e.g. %s=%s@ghcr.io/you)", name, t.Context, name, t.Context)
		}
		out = append(out, t)
	}
	return out, nil
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Discover lists the local clusters in the kubeconfig as targets, each
// deploying to the fliessband namespace.
func Discover(ctx context.Context, env []string) []Target {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", "config", "get-contexts", "-o", "name")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var ts []Target
	for c := range strings.FieldsSeq(string(out)) {
		s, cluster, local := detect(c)
		if !local {
			continue
		}
		name := strings.ToLower(c)
		if !targetName.MatchString(name) {
			continue
		}
		ts = append(ts, Target{Name: name, Context: c, Namespace: DefaultNamespace, Strategy: s, Cluster: cluster, Local: true})
	}
	return ts
}
