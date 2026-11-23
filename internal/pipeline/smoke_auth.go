package pipeline

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/tillknuesting/fliessband/internal/gen"
	"github.com/tillknuesting/fliessband/internal/spec"
)

// loadMethod picks the RPC the smoke station load-tests with empty
// requests: the first List RPC anonymous callers may call, else health.
func loadMethod(model *gen.Model) string {
	for _, e := range model.Entities {
		if !e.HasList {
			continue
		}
		rpc := "List" + e.Plural
		if model.Auth == nil || ruleKind(model.Auth, rpc) == spec.AccessPublic {
			return "/" + model.FullServiceName + "/" + rpc
		}
	}
	return "/grpc.health.v1.Health/Check"
}

func ruleKind(a *gen.AuthModel, rpc string) string {
	for _, r := range a.Rules {
		if r.RPC == rpc {
			return r.Kind
		}
	}
	return a.DefaultKind
}

// smokeAuth checks the running binary refuses a call without credentials to
// an RPC that needs them. The smoke station holds no token, so this is the
// half of access control it can check from outside.
func (x *executor) smokeAuth(ctx context.Context, st *StageState, conn *grpc.ClientConn, model *gen.Model) error {
	if model.Auth == nil {
		return nil
	}
	r := model.Auth.ProtectedRPC()
	if r == nil {
		x.log(st, "auth: every RPC is public or denied; nothing needs credentials")
		return nil
	}
	err := conn.Invoke(ctx, r.Method, &emptypb.Empty{}, &emptypb.Empty{})
	if status.Code(err) != codes.Unauthenticated {
		return fmt.Errorf("%s without credentials returned %v, want Unauthenticated", r.RPC, status.Code(err))
	}
	x.log(st, fmt.Sprintf("auth: %s without credentials is refused with UNAUTHENTICATED", r.RPC))
	return nil
}
