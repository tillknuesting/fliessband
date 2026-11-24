package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tillknuesting/fliessband/internal/deploy"
)

func TestDeployEndpoints(t *testing.T) {
	// Off: the UI learns it from the state and hides the button.
	h := newTestHandler(t, Config{LoopbackOnly: true})
	w := do(h, "GET", "http://localhost:7070/api/deploy", nil)
	var st struct {
		Enabled bool `json:"enabled"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &st) != nil || st.Enabled {
		t.Fatalf("deploy state when off: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "POST", "http://localhost:7070/api/runs/x/deploy", nil); w.Code != http.StatusNotImplemented {
		t.Errorf("deploy when off: %d", w.Code)
	}

	d, err := deploy.New(t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h = newTestHandler(t, Config{LoopbackOnly: true, Deployer: d})
	if w := do(h, "GET", "http://localhost:7070/api/deploy", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"enabled":true`) {
		t.Errorf("deploy state: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "POST", "http://localhost:7070/api/runs/nope/deploy", nil); w.Code != http.StatusNotFound {
		t.Errorf("deploying an unknown run: %d", w.Code)
	}
	if w := do(h, "GET", "http://localhost:7070/api/deployments/nope", nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown deployment: %d", w.Code)
	}
	if w := do(h, "POST", "http://localhost:7070/api/deploy/nope/inventory/remove", nil); w.Code != http.StatusNotFound {
		t.Errorf("removing from an unknown target: %d", w.Code)
	}
	// Writes are CSRF-protected like the rest of the API.
	w = do(h, "POST", "http://localhost:7070/api/deploy/nope/inventory/remove", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") })
	if w.Code != http.StatusForbidden {
		t.Errorf("cross-site remove: %d", w.Code)
	}
}
