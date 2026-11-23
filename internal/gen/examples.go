package gen

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/tillknuesting/fliessband/internal/spec"
)

// ExampleModel is one acceptance example rendered into acceptance_test.go.
type ExampleModel struct {
	Method  string
	Name    string
	Holdout bool
	Given   []SeedModel
	Request string // JSON with ${...} references
	Expect  []ExpectModel
	Code    string // codes.X, codes.OK when the call must succeed
}

// SeedModel is one record to create before the call.
type SeedModel struct{ Entity, JSON string }

// ExpectModel is one expected response value, as JSON.
type ExpectModel struct{ Path, Want string }

// codeNames maps proto code names to grpc-go constant names.
var codeNames = map[string]string{
	"CANCELLED": "Canceled", "UNKNOWN": "Unknown", "INVALID_ARGUMENT": "InvalidArgument",
	"DEADLINE_EXCEEDED": "DeadlineExceeded", "NOT_FOUND": "NotFound", "ALREADY_EXISTS": "AlreadyExists",
	"PERMISSION_DENIED": "PermissionDenied", "RESOURCE_EXHAUSTED": "ResourceExhausted",
	"FAILED_PRECONDITION": "FailedPrecondition", "ABORTED": "Aborted", "OUT_OF_RANGE": "OutOfRange",
	"UNIMPLEMENTED": "Unimplemented", "INTERNAL": "Internal", "UNAVAILABLE": "Unavailable",
	"DATA_LOSS": "DataLoss", "UNAUTHENTICATED": "Unauthenticated",
}

// jsonValue makes YAML-decoded values JSON-ready: timestamps become RFC 3339
// strings and nested maps get string keys.
func jsonValue(v any) any {
	switch t := v.(type) {
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	case []any:
		out := make([]any, len(t))
		for i, el := range t {
			out[i] = jsonValue(el)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, el := range t {
			out[k] = jsonValue(el)
		}
		return out
	}
	return v
}

func mustJSON(v any) string {
	b, err := json.Marshal(jsonValue(v))
	if err != nil {
		panic(err) // values come from YAML, so they always marshal
	}
	return string(b)
}

func newExamples(s *spec.Spec) []*ExampleModel {
	var out []*ExampleModel
	for _, m := range s.Methods {
		for _, ex := range m.Examples {
			em := &ExampleModel{Method: m.Name, Name: ex.Name, Holdout: ex.Holdout, Code: "codes.OK"}
			if ex.Error != "" {
				em.Code = "codes." + codeNames[ex.Error]
			}
			for _, ent := range s.GivenOrder(ex) {
				for _, rec := range ex.Given[ent] {
					em.Given = append(em.Given, SeedModel{Entity: ent, JSON: mustJSON(rec)})
				}
			}
			req := ex.Request
			if req == nil {
				req = map[string]any{}
			}
			em.Request = mustJSON(req)
			paths := make([]string, 0, len(ex.Expect))
			for p := range ex.Expect {
				paths = append(paths, p)
			}
			slices.Sort(paths)
			for _, p := range paths {
				em.Expect = append(em.Expect, ExpectModel{Path: p, Want: mustJSON(ex.Expect[p])})
			}
			out = append(out, em)
		}
	}
	return out
}

// ExampleTestName is the go test name of an example, as the testing package
// reports it (spaces become underscores).
func ExampleTestName(method, name string) string {
	return "TestAcceptance/" + method + "/" + strings.ReplaceAll(name, " ", "_")
}
