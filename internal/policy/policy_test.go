package policy

import (
	"strings"
	"testing"
)

var cfg = Config{Module: "example.com/tasks", PBPackage: "example.com/tasks/gen/tasks/v1"}

func rules(vs []Violation) []string {
	var out []string
	for _, v := range vs {
		out = append(out, v.Rule)
	}
	return out
}

func TestCleanFilesPass(t *testing.T) {
	files := map[string][]byte{
		"internal/service/custom.go": []byte(`package service

import (
	"context"
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "example.com/tasks/gen/tasks/v1"
	"example.com/tasks/internal/auth"
	"example.com/tasks/internal/store"
)

var _ = store.ErrNotFound
var _ = auth.FromContext
var _ = slices.Contains[[]int]
var _ = timestamppb.Now

func (s *Server) CompleteTask(ctx context.Context, req *pb.CompleteTaskRequest) (*pb.CompleteTaskResponse, error) {
	return nil, status.Error(codes.NotFound, "x")
}
`),
		"internal/service/custom_test.go": []byte(`package service

import "testing"

func TestCompleteTask(t *testing.T) {
	c, _ := newClient(t)
	_, err := c.CompleteTask(t.Context(), nil)
	wantCode(t, err, 5)
}

func TestHelperStyle(t *testing.T) {
	if 1+1 != 2 {
		t.Fatalf("math is broken")
	}
}
`),
	}
	if vs := Check(cfg, files); len(vs) != 0 {
		t.Fatalf("clean files flagged: %v", vs)
	}
}

func TestViolations(t *testing.T) {
	files := map[string][]byte{
		"internal/service/custom.go": []byte(`//go:build linux

package service

import (
	"net/http"
	"os/exec"
	. "strings"
)

func init() {}

func (s *Server) Exfiltrate() {
	_ = exec.Command("curl")
	_, _ = http.Get("http://example.com")
	_ = ToUpper("x")
	panic("no")
}
`),
		"internal/service/custom_test.go": []byte(`package service

import (
	"os"
	"testing"
)

func TestNothing(t *testing.T) {
	_ = os.Getenv("HOME")
}

func TestSkipped(t *testing.T) {
	t.Skip("flaky")
	t.Fatal("unreachable")
}
`),
	}
	vs := Check(cfg, files)
	got := strings.Join(rules(vs), ",")
	for _, want := range []string{"directive", "import", "dot-import", "init", "panic", "assert", "skip"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s violation; got %s", want, got)
		}
	}
	for _, v := range vs {
		if v.Rule == "import" && !strings.Contains(v.Message, "Allowed:") {
			t.Errorf("import violation should list the allowed packages: %s", v.Message)
		}
		if v.Line == 0 || v.File == "" {
			t.Errorf("violation without position: %+v", v)
		}
	}
}

func TestSyntaxError(t *testing.T) {
	vs := Check(cfg, map[string][]byte{"internal/service/custom.go": []byte("package service\nfunc {")})
	if len(vs) != 1 || vs[0].Rule != "syntax" {
		t.Fatalf("got %v", vs)
	}
}
