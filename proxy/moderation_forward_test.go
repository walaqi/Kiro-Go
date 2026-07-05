package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
)

func TestRewriteForwardBodyMinimizesToLatestUser(t *testing.T) {
	// A multi-turn body with history, system, tools, and a custom field. Only the
	// latest user message + whitelisted params should survive; everything else is
	// dropped for data minimization.
	raw := []byte(`{
		"model":"claude-sonnet-4",
		"max_tokens":100,
		"temperature":0.7,
		"system":"you are a helpful assistant",
		"tools":[{"name":"calc","description":"math"}],
		"custom_field":"drop-me",
		"messages":[
			{"role":"user","content":"first question"},
			{"role":"assistant","content":"first answer"},
			{"role":"user","content":"latest question"}
		]
	}`)
	out, err := rewriteForwardBody(raw, "gpt-origin-model", false)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// model swapped to origin
	if body["model"] != "gpt-origin-model" {
		t.Fatalf("model not swapped: %v", body["model"])
	}
	// only the latest user message remains
	msgs, ok := body["messages"].([]interface{})
	if !ok || len(msgs) != 1 {
		t.Fatalf("expected exactly 1 message, got %v", body["messages"])
	}
	m0 := msgs[0].(map[string]interface{})
	if m0["role"] != "user" || m0["content"] != "latest question" {
		t.Fatalf("expected only the latest user message, got %v", m0)
	}
	// whitelisted params copied verbatim
	if body["max_tokens"].(float64) != 100 {
		t.Fatalf("max_tokens changed: %v", body["max_tokens"])
	}
	if body["temperature"].(float64) != 0.7 {
		t.Fatalf("temperature changed: %v", body["temperature"])
	}
	// history / system / tools / custom fields dropped
	if _, ok := body["system"]; ok {
		t.Fatalf("system prompt must be dropped, got %v", body["system"])
	}
	if _, ok := body["tools"]; ok {
		t.Fatalf("tools must be dropped, got %v", body["tools"])
	}
	if _, ok := body["custom_field"]; ok {
		t.Fatalf("custom_field must be dropped, got %v", body["custom_field"])
	}
}

func TestRewriteForwardBodyNoUserMessage(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"assistant","content":"hi"}]}`)
	if _, err := rewriteForwardBody(raw, "origin", false); err == nil {
		t.Fatal("expected error when there is no user message to forward")
	}
}

func TestRewriteForwardBodyInjectsDefaultMaxTokens(t *testing.T) {
	// Downstream omits max_tokens (Kiro-Go's inbound validation allows this).
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	out, err := rewriteForwardBody(raw, "origin", false)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["max_tokens"].(float64) != float64(defaultForwardMaxTokens) {
		t.Fatalf("expected default max_tokens %d, got %v", defaultForwardMaxTokens, body["max_tokens"])
	}
}

func TestRewriteForwardBodyPreservesExplicitMaxTokens(t *testing.T) {
	// When the downstream DID send max_tokens, it must be preserved, not overridden.
	raw := []byte(`{"model":"m","max_tokens":500,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := rewriteForwardBody(raw, "origin", false)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["max_tokens"].(float64) != 500 {
		t.Fatalf("explicit max_tokens must be preserved, got %v", body["max_tokens"])
	}
}

func TestRewriteForwardBodyFullContentPreservesEverything(t *testing.T) {
	// fullContent=true: history, system, tools, and custom fields must all survive;
	// only the model is swapped.
	raw := []byte(`{
		"model":"claude-sonnet-4",
		"max_tokens":100,
		"system":"you are a helpful assistant",
		"tools":[{"name":"calc","description":"math"}],
		"custom_field":"keep-me",
		"messages":[
			{"role":"user","content":"first question"},
			{"role":"assistant","content":"first answer"},
			{"role":"user","content":"latest question"}
		]
	}`)
	out, err := rewriteForwardBody(raw, "gpt-origin", true)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// model swapped
	if body["model"] != "gpt-origin" {
		t.Fatalf("model not swapped: %v", body["model"])
	}
	// full history preserved (all 3 messages)
	msgs, ok := body["messages"].([]interface{})
	if !ok || len(msgs) != 3 {
		t.Fatalf("expected all 3 messages preserved, got %v", body["messages"])
	}
	// system / tools / custom fields all kept
	if body["system"] != "you are a helpful assistant" {
		t.Fatalf("system must be preserved, got %v", body["system"])
	}
	if _, ok := body["tools"]; !ok {
		t.Fatalf("tools must be preserved")
	}
	if body["custom_field"] != "keep-me" {
		t.Fatalf("custom_field must be preserved, got %v", body["custom_field"])
	}
	if body["max_tokens"].(float64) != 100 {
		t.Fatalf("max_tokens changed: %v", body["max_tokens"])
	}
}

// TestRewriteForwardBodyPreservesLargeIntegers is the regression guard for the
// float64 round-trip bug: parsing the body into map[string]interface{} normalizes
// every JSON number to float64, corrupting large integers (IDs, timestamps) in
// metadata / custom fields. The RawMessage-based rewrite must reproduce them
// byte-for-byte in BOTH modes. We assert on raw bytes, not decoded values, since
// decoding here would hide the very corruption we're guarding against.
func TestRewriteForwardBodyPreservesLargeIntegers(t *testing.T) {
	const bigInt = "9007199254740993" // 2^53 + 1, not representable exactly as float64
	raw := []byte(`{"model":"m","max_tokens":10,"metadata":{"request_id":` + bigInt + `},"messages":[{"role":"user","content":"hi"}]}`)

	for _, fullContent := range []bool{true, false} {
		out, err := rewriteForwardBody(raw, "origin", fullContent)
		if err != nil {
			t.Fatalf("fullContent=%v: rewrite: %v", fullContent, err)
		}
		if !strings.Contains(string(out), bigInt) {
			t.Fatalf("fullContent=%v: large integer %s corrupted, got: %s", fullContent, bigInt, string(out))
		}
	}
}

// forwardTestHandler builds a handler and a ready moderation config forwarding to
// target, plus an opted-in key. Returns the handler, key ID.
func forwardTestHandler(t *testing.T, target string) (*Handler, string) {
	t.Helper()
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateModerationConfig(config.ModerationConfig{
		Enabled:    true,
		JudgeModel: "claude-haiku-4.5",
		Rules:      []config.JudgeRule{{ID: "r1", Enabled: true, Criteria: "cyber attack"}},
		ForwardURL: target,
		ForwardKey: "fk-secret-abcdef",
	}); err != nil {
		t.Fatalf("UpdateModerationConfig: %v", err)
	}
	entry, err := config.AddApiKey(config.ApiKeyEntry{Key: "downstream-key-999", Enabled: true, Moderation: true})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}
	h := &Handler{
		judgeCallOverride: func(model, prompt string) (string, error) { return "1", nil }, // always hit
	}
	return h, entry.ID
}

func TestForwardStripsOriginalHeadersAndSetsForwardKey(t *testing.T) {
	var gotAuth, gotXApiKey, gotOriginModel, gotCT string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotXApiKey = r.Header.Get("x-api-key")
		gotOriginModel = r.Header.Get("X-Origin-Model-Id")
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer target.Close()

	h, keyID := forwardTestHandler(t, target.URL)

	// Build a downstream request carrying its OWN Authorization/x-api-key that must
	// NOT be forwarded, plus the control header X-Origin-Model-Id.
	rawBody := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"attack"}]}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(rawBody)))
	r.Header.Set("Authorization", "Bearer downstream-secret-DO-NOT-FORWARD")
	r.Header.Set("x-api-key", "downstream-key-DO-NOT-FORWARD")
	r.Header.Set("X-Origin-Model-Id", "claude-opus-4")
	if entry := config.GetApiKeyEntry(keyID); entry != nil {
		r = withApiKeyContext(r, entry)
	}

	rec := httptest.NewRecorder()
	if !h.maybeModerate(rec, r, &ClaudeRequest{
		Model:    "claude-sonnet-4",
		Messages: []ClaudeMessage{{Role: "user", Content: "attack"}},
	}, rawBody) {
		t.Fatal("expected hit → forwarded (handled=true)")
	}

	// Forward key must be present as a Bearer token; downstream secrets and the
	// x-api-key scheme must be gone (single-auth-scheme: Bearer only).
	if gotAuth != "Bearer fk-secret-abcdef" {
		t.Fatalf("Authorization not the forward key: %q", gotAuth)
	}
	if gotXApiKey != "" {
		t.Fatalf("x-api-key must not be sent (Bearer-only auth), got %q", gotXApiKey)
	}
	if strings.Contains(gotAuth, "DO-NOT-FORWARD") {
		t.Fatalf("downstream Authorization leaked: %q", gotAuth)
	}
	if gotOriginModel != "" {
		t.Fatalf("control header X-Origin-Model-Id must not be forwarded, got %q", gotOriginModel)
	}
	if gotCT != "application/json" {
		t.Fatalf("Content-Type not set on forward: %q", gotCT)
	}
	if rec.Code != 200 {
		t.Fatalf("status not passed through: %d", rec.Code)
	}
}

func TestForwardBodyModelSwappedToOrigin(t *testing.T) {
	var receivedModel string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		receivedModel, _ = body["model"].(string)
		w.WriteHeader(200)
	}))
	defer target.Close()

	h, keyID := forwardTestHandler(t, target.URL)
	rawBody := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"attack"}]}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(rawBody)))
	r.Header.Set("X-Origin-Model-Id", "gpt-4-origin")
	if entry := config.GetApiKeyEntry(keyID); entry != nil {
		r = withApiKeyContext(r, entry)
	}
	rec := httptest.NewRecorder()
	h.maybeModerate(rec, r, &ClaudeRequest{
		Model:    "claude-sonnet-4",
		Messages: []ClaudeMessage{{Role: "user", Content: "attack"}},
	}, rawBody)

	if receivedModel != "gpt-4-origin" {
		t.Fatalf("forwarded body model should be origin model, got %q", receivedModel)
	}
}

func TestForwardStatusAndContentTypePassthrough(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(418) // distinctive status to confirm passthrough
		_, _ = w.Write([]byte("data: hi\n\n"))
	}))
	defer target.Close()

	h, keyID := forwardTestHandler(t, target.URL)
	rawBody := []byte(`{"model":"m","messages":[{"role":"user","content":"attack"}]}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(rawBody)))
	r.Header.Set("X-Origin-Model-Id", "m-origin")
	if entry := config.GetApiKeyEntry(keyID); entry != nil {
		r = withApiKeyContext(r, entry)
	}
	rec := httptest.NewRecorder()
	h.maybeModerate(rec, r, &ClaudeRequest{Messages: []ClaudeMessage{{Role: "user", Content: "attack"}}}, rawBody)

	if rec.Code != 418 {
		t.Fatalf("status not passed through: %d", rec.Code)
	}
	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("Content-Type not passed through: %q", rec.Header().Get("Content-Type"))
	}
}

// TestForwardCancellationPropagates verifies that when the downstream request's
// context is cancelled (client disconnected), the forward to the target is torn
// down — the target observes ctx.Done rather than the request running forever.
func TestForwardCancellationPropagates(t *testing.T) {
	targetSawCancel := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		// Stream one chunk, then block until the client's context cancels.
		_, _ = w.Write([]byte("data: start\n\n"))
		if fl != nil {
			fl.Flush()
		}
		select {
		case <-r.Context().Done():
			targetSawCancel <- struct{}{}
		case <-time.After(5 * time.Second):
		}
	}))
	defer target.Close()

	h, keyID := forwardTestHandler(t, target.URL)
	rawBody := []byte(`{"model":"m","messages":[{"role":"user","content":"attack"}]}`)

	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(rawBody)))
	r = r.WithContext(ctx)
	r.Header.Set("X-Origin-Model-Id", "m-origin")
	if entry := config.GetApiKeyEntry(keyID); entry != nil {
		r = withApiKeyContext(r, entry)
	}

	// Use a pipe-backed writer so the forward goroutine can stream while we cancel.
	pr, pw := io.Pipe()
	rw := &flushRecorder{w: pw}
	done := make(chan struct{})
	go func() {
		h.maybeModerate(rw, r, &ClaudeRequest{Messages: []ClaudeMessage{{Role: "user", Content: "attack"}}}, rawBody)
		close(done)
	}()

	// Wait for the first streamed chunk to arrive downstream, then cancel.
	br := bufio.NewReader(pr)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("expected first streamed chunk: %v", err)
	}
	cancel()

	select {
	case <-targetSawCancel:
		// good: cancellation propagated to the forward target
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not propagate to forward target")
	}
	_ = pw.CloseWithError(io.EOF)
	<-done
}

// flushRecorder is a minimal http.ResponseWriter+Flusher backed by an io.Writer,
// used so streaming forward output can be consumed live in the cancellation test.
type flushRecorder struct {
	w      io.Writer
	header http.Header
	status int
}

func (f *flushRecorder) Header() http.Header {
	if f.header == nil {
		f.header = make(http.Header)
	}
	return f.header
}
func (f *flushRecorder) Write(b []byte) (int, error) { return f.w.Write(b) }
func (f *flushRecorder) WriteHeader(status int)      { f.status = status }
func (f *flushRecorder) Flush()                      {}
