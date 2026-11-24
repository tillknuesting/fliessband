package llm

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTokenLimitAndCutOff(t *testing.T) {
	var got map[string]any
	finish := "stop"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": "[]"}, "finish_reason": finish}},
		})
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, APIKey: "k", Model: "m", HTTP: srv.Client()}

	if _, _, err := c.Chat(t.Context(), "", []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	if got["max_tokens"] != float64(DefaultMaxTokens) {
		t.Errorf("max_tokens = %v, want %d", got["max_tokens"], DefaultMaxTokens)
	}
	if _, _, err := c.Review(t.Context(), "m", Context{Owned: map[string]string{"a.go": "package a"}}); err != nil {
		t.Fatal(err)
	}
	if got["max_tokens"] != float64(reviewMaxTokens) {
		t.Errorf("review max_tokens = %v, want %d", got["max_tokens"], reviewMaxTokens)
	}
	if _, ok := got["reasoning"]; ok {
		t.Error("reasoning was sent to a provider that is not OpenRouter")
	}

	c.MaxTokens = 500
	finish = "length"
	_, _, err := c.Chat(t.Context(), "", []Message{{Role: "user", Content: "hi"}})
	if err == nil || !strings.Contains(err.Error(), "500-token limit") {
		t.Errorf("err = %v, want a cut-off error naming the limit", err)
	}
	if got["max_tokens"] != float64(500) {
		t.Errorf("max_tokens = %v, want the configured 500", got["max_tokens"])
	}
}
