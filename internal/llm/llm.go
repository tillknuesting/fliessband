// Package llm implements the factory's constrained code-writing stage: it asks
// an OpenAI-compatible chat model to implement custom RPC stubs, and to repair
// them from compiler or test output. The model may only write the files the
// factory allows; everything else it returns is rejected.
package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/format"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Client calls an OpenAI-compatible /chat/completions endpoint.
type Client struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    *http.Client
}

// FromEnv builds a client from FLIESSBAND_LLM_* or OPENAI_* variables. It returns
// nil when no API key is set, which turns the implement stage off.
//
//	FLIESSBAND_LLM_BASE_URL / OPENAI_BASE_URL  (default https://api.openai.com/v1)
//	FLIESSBAND_LLM_API_KEY  / OPENAI_API_KEY
//	FLIESSBAND_LLM_MODEL    (default chosen from the base URL)
func FromEnv() *Client {
	key := first(os.Getenv("FLIESSBAND_LLM_API_KEY"), os.Getenv("OPENAI_API_KEY"))
	if key == "" {
		return nil
	}
	base := strings.TrimRight(first(os.Getenv("FLIESSBAND_LLM_BASE_URL"), os.Getenv("OPENAI_BASE_URL"), "https://api.openai.com/v1"), "/")
	return &Client{
		BaseURL: base,
		APIKey:  key,
		Model:   first(os.Getenv("FLIESSBAND_LLM_MODEL"), defaultModel(base)),
		HTTP:    &http.Client{Timeout: 6 * time.Minute},
	}
}

func defaultModel(base string) string {
	switch {
	case strings.Contains(base, "deepseek"):
		return "deepseek-chat"
	case strings.Contains(base, "openrouter"):
		return "openai/gpt-5-mini"
	case strings.Contains(base, "z.ai"), strings.Contains(base, "bigmodel"):
		return "glm-4.6"
	}
	return "gpt-5-mini"
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Message is one chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Usage counts tokens of one call. CachedTokens are prompt tokens the
// provider served from its prompt cache (OpenAI reports them as
// prompt_tokens_details.cached_tokens, DeepSeek as prompt_cache_hit_tokens).
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	CachedTokens     int `json:"-"`
}

type usageJSON struct {
	Usage
	Details struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CacheHit int `json:"prompt_cache_hit_tokens"`
}

// Chat sends messages and returns the first choice's content.
func (c *Client) Chat(ctx context.Context, model string, msgs []Message) (string, Usage, error) {
	if model == "" {
		model = c.Model
	}
	body, _ := json.Marshal(map[string]any{"model": model, "messages": msgs})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", Usage{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", Usage{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return "", Usage{}, fmt.Errorf("LLM API %s: %s", resp.Status, truncate(string(raw), 500))
	}
	var out struct {
		Choices []struct {
			Message Message `json:"message"`
		} `json:"choices"`
		Usage usageJSON `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", Usage{}, fmt.Errorf("decode LLM response: %w", err)
	}
	u := out.Usage.Usage
	u.CachedTokens = max(out.Usage.Details.CachedTokens, out.Usage.CacheHit)
	if len(out.Choices) == 0 {
		return "", u, errors.New("LLM returned no choices")
	}
	return out.Choices[0].Message.Content, u, nil
}

// Context is what the model sees about the service.
type Context struct {
	Proto       string            // the .proto file
	Files       map[string]string // path -> content of reference files
	Owned       map[string]string // path -> current content of files it may write
	MethodBrief string            // the custom methods and their docs
}

const system = `You are a senior Go engineer inside a software factory that generates gRPC microservices.

Rules:
- You may write only the files listed under "Files you own". Reply with the complete new content of each of them, every file in its own fenced block whose info string is exactly: go path=<file path>. Write nothing else inside or between the blocks.
- Keep package service and keep every method signature exactly as in the stubs. Remove the "fliessband:implement" markers.
- Imports: the standard library packages bytes, cmp, context, encoding/base64, encoding/json, errors, fmt, iter, log/slog, maps, math, regexp, slices, sort, strconv, strings, sync, sync/atomic, time, unicode, unicode/utf8 and uuid; in tests also testing. Beyond that only grpc codes and status, errdetails, protobuf proto and known types, the generated pb package and the internal store package. The factory rejects any other import before compiling, and also init functions, panics and compiler directives.
- Return gRPC status errors (google.golang.org/grpc/status and codes). Convert store errors with s.toStatus(ctx, err). For bad input use the violations type (v.add(field, msg); return v.err()) when it exists, otherwise status.Error(codes.InvalidArgument, ...).
- For read-modify-write use the store's Update<Entity>(ctx, id, mutate) so the change is atomic; never call the store inside mutate. To scan, call List<Entities>(ctx, after, limit) page by page: start with after "", pass the last id of each page as the next after, and stop at a page shorter than limit.
- Tests: table-driven where it helps. Use newClient(t) from service_test.go (it returns a client and the *store.Memory), wantCode, wantViolation, and valid<Entity>(i) / seed<Entity>(t, c, st, i) from the entity test files. Cover the success path and every error the method doc describes. Every test must assert; never skip a test. Use t.Context().
- The factory also runs acceptance examples from the spec as tests you cannot see or change, and some of them are hidden from you. Implement the documented behaviour in general, not only the examples shown.
- The code must compile and pass go vet, golangci-lint and go test -race.`

// Result is the outcome of one model call.
type Result struct {
	Files      map[string]string
	Usage      Usage
	Rejected   []string // paths the model tried to write but does not own
	Equivalent []int    // mutant ids the model judged impossible to detect
	PromptSHA  string   // digest of the prompt, for provenance
}

// Implement asks the model to replace the stubs in the owned files.
func (c *Client) Implement(ctx context.Context, model string, in Context) (Result, error) {
	var b strings.Builder
	writeContext(&b, in)
	b.WriteString("\n## Task\n\nImplement the custom RPCs of this service and their tests.\n\n")
	b.WriteString(in.MethodBrief)
	return c.ask(ctx, model, b.String(), sortedKeys(in.Owned))
}

// Repair asks the model to fix the owned files given a failing tool's output.
func (c *Client) Repair(ctx context.Context, model string, in Context, stage, output string) (Result, error) {
	var b strings.Builder
	writeContext(&b, in)
	fmt.Fprintf(&b, "\n## Task\n\nYour files fail the %q station. Fix them.\n\n### Tool output\n\n```\n%s\n```\n\n", stage, truncate(output, 12000))
	b.WriteString(in.MethodBrief)
	return c.ask(ctx, model, b.String(), sortedKeys(in.Owned))
}

// Harden asks the model for tests that detect the surviving mutants of its
// own code (mutation-guided test generation, as in Meta's ACH). Only the
// test file may change. The model may name mutants it judges equivalent.
func (c *Client) Harden(ctx context.Context, model string, in Context, testFile, survivors string) (Result, error) {
	var b strings.Builder
	writeContext(&b, in)
	fmt.Fprintf(&b, "\n## Task\n\nYour implementation passes its tests, but these mutants (small deliberate faults) of your code survive: the tests still pass with them. "+
		"Add or strengthen tests in %s so that each mutant makes a test fail. Do not change any other file: reply with only %s. "+
		"If a mutant cannot be detected by any test because it does not change behaviour, do not write a test for it; instead add a line \"EQUIVALENT: <id>, <id>\" after the code block.\n\n### Surviving mutants\n\n%s\n", testFile, testFile, survivors)
	b.WriteString(in.MethodBrief)
	return c.ask(ctx, model, b.String(), []string{testFile})
}

// writeContext writes the parts of the prompt that stay the same across
// calls first, so providers with prefix caching can reuse them.
func writeContext(b *strings.Builder, in Context) {
	fmt.Fprintf(b, "## API contract\n\n```proto\n%s\n```\n", in.Proto)
	b.WriteString("\n## Reference files (read only)\n")
	for _, p := range sortedKeys(in.Files) {
		fmt.Fprintf(b, "\n### %s\n\n```go\n%s\n```\n", p, in.Files[p])
	}
	b.WriteString("\n## Files you own\n")
	for _, p := range sortedKeys(in.Owned) {
		fmt.Fprintf(b, "\n### %s\n\n```go\n%s\n```\n", p, in.Owned[p])
	}
}

var reEquivalent = regexp.MustCompile(`(?m)^EQUIVALENT:\s*([#0-9,\s]+)$`)

func (c *Client) ask(ctx context.Context, model, prompt string, allowed []string) (Result, error) {
	sum := sha256.Sum256([]byte(system + "\n" + prompt))
	res := Result{PromptSHA: hex.EncodeToString(sum[:])}
	reply, usage, err := c.Chat(ctx, model, []Message{{Role: "system", Content: system}, {Role: "user", Content: prompt}})
	res.Usage = usage
	if err != nil {
		return res, err
	}
	res.Files, res.Rejected = ParseFiles(reply, allowed)
	for _, m := range reEquivalent.FindAllStringSubmatch(reply, -1) {
		for _, f := range strings.FieldsFunc(m[1], func(r rune) bool { return r == ',' || r == ' ' || r == '#' || r == '\n' }) {
			if id, err := strconv.Atoi(f); err == nil {
				res.Equivalent = append(res.Equivalent, id)
			}
		}
	}
	if len(res.Files) == 0 {
		return res, fmt.Errorf("the model returned none of the files it may write; reply started with: %s", truncate(reply, 300))
	}
	return res, nil
}

var reBlock = regexp.MustCompile("(?s)```go[ \\t]+path=([^\\s`]+)[^\\n]*\\n(.*?)\\n```")

// ParseFiles extracts "```go path=..." blocks, keeps only allowed paths and
// gofmts them. Disallowed paths are returned as rejected.
func ParseFiles(reply string, allowed []string) (files map[string]string, rejected []string) {
	files = map[string]string{}
	for _, m := range reBlock.FindAllStringSubmatch(reply, -1) {
		path, body := strings.TrimPrefix(m[1], "./"), m[2]+"\n"
		if !slices.Contains(allowed, path) {
			rejected = append(rejected, path)
			continue
		}
		if formatted, err := format.Source([]byte(body)); err == nil {
			body = string(formatted)
		}
		files[path] = body
	}
	return files, rejected
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…(truncated)"
}
