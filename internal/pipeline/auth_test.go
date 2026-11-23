package pipeline

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestAuthLine runs the bookings example, which has JWT auth and per-RPC
// rules, through the real line without an LLM: the generated service with
// its auth package must build, vet, lint clean and pass its own tests, the
// acceptance example that calls without a role must be refused, the
// running binary must refuse calls without a token, and the security audit
// must find access control in place.
func TestAuthLine(t *testing.T) {
	toolsDir := tools(t)
	src, err := os.ReadFile("../../specs/bookings.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(t.TempDir(), toolsDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.Start(src, Options{Smoke: true, Mutation: true, MaxMutants: 60})
	if err != nil {
		t.Fatal(err)
	}
	m.Wait(r.ID)
	var got Run
	if err := json.Unmarshal(r.JSON(), &got); err != nil {
		t.Fatal(err)
	}
	for _, st := range got.Stages {
		t.Logf("%-12s %-8s %s", st.ID, st.Status, st.Summary)
	}
	if got.Status != Passed {
		t.Fatalf("run status = %s, want passed", got.Status)
	}
	for _, id := range []string{"deps", "build", "vet", "test", "acceptance", "smoke"} {
		if st := got.stage(id); st == nil || st.Status != Passed {
			t.Errorf("station %s = %+v, want passed", id, st)
		}
	}
	rep := got.Report
	for _, f := range rep.Findings.Lint {
		if strings.Contains(f.File, "auth") {
			t.Errorf("lint finding in the auth code: %+v", f)
		}
	}
	acc := rep.Acceptance
	if acc == nil {
		t.Fatal("no acceptance report")
	}
	if acc.Pending != len(acc.Examples) {
		t.Errorf("acceptance = %+v, want every example pending while CheckAvailability is a stub", acc)
	}
	// The line skips the examples of stubbed methods. The one that calls
	// without a role is refused by the interceptor before the stub runs,
	// so it must pass already.
	cmd := exec.Command("go", "test", "-count=1", "-run", "^TestAcceptance$/^CheckAvailability$/^needs_a_role$", "-v", "./internal/service")
	cmd.Dir = r.ServiceDir()
	cmd.Env = m.childEnv(false)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: TestAcceptance/CheckAvailability/needs_a_role") {
		t.Errorf("the example without a role does not pass: %v\n%s", err, out)
	}
	if rt := rep.Runtime; rt == nil || !strings.HasSuffix(rt.LoadMethod, "/ListRooms") || rt.LoadErrors != 0 {
		t.Errorf("runtime = %+v, want an error-free load test on the public ListRooms", rt)
	}
	if !strings.Contains(strings.Join(got.stage("smoke").Log, "\n"), "refused with UNAUTHENTICATED") {
		t.Error("the smoke station did not check a call without a token")
	}
	for _, c := range rep.Security.Checks {
		if c.Area == "Access control" && c.Status != "pass" {
			t.Errorf("security check %s = %s: %s", c.ID, c.Status, c.Detail)
		}
	}
	if mut := rep.Mutation; mut != nil {
		for _, s := range mut.Survivors {
			if strings.HasPrefix(s.File, "internal/auth/") {
				t.Logf("surviving mutant %s:%d %s: %s -> %s", s.File, s.Line, s.Operator, s.Before, s.After)
			}
		}
	}
}
