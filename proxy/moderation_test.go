package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
)

// --- judge engine: verdict parsing & rule filtering ---

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name      string
		reply     string
		ruleCount int
		wantHit   bool
		wantMatch []int
	}{
		{"explicit none", "0", 3, false, nil},
		{"single hit", "2", 3, true, []int{2}},
		{"multi hit", "1,3", 3, true, []int{1, 3}},
		{"noisy prose", "Rules 1 and 3 are violated.", 3, true, []int{1, 3}},
		{"out of range ignored", "5", 3, false, nil},
		{"mixed in/out of range", "2, 9", 3, true, []int{2}},
		{"dedupe", "2 2 2", 3, true, []int{2}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hit, matched := parseVerdict(c.reply, c.ruleCount)
			if hit != c.wantHit {
				t.Fatalf("hit=%v want %v (reply=%q)", hit, c.wantHit, c.reply)
			}
			if len(matched) != len(c.wantMatch) {
				t.Fatalf("matched=%v want %v", matched, c.wantMatch)
			}
			for i := range matched {
				if matched[i] != c.wantMatch[i] {
					t.Fatalf("matched=%v want %v", matched, c.wantMatch)
				}
			}
		})
	}
}

func TestTruncateForLog(t *testing.T) {
	// Short text is returned trimmed, unchanged.
	if got := truncateForLog("  hello  "); got != "hello" {
		t.Fatalf("expected trimmed 'hello', got %q", got)
	}
	// Long ASCII is clipped to the rune cap plus ellipsis.
	long := strings.Repeat("a", moderationLogMaxRunes+50)
	got := truncateForLog(long)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected ellipsis suffix, got %q", got)
	}
	if len([]rune(got)) != moderationLogMaxRunes+1 { // +1 for the ellipsis rune
		t.Fatalf("expected %d runes, got %d", moderationLogMaxRunes+1, len([]rune(got)))
	}
	// Multibyte (Chinese) text is clipped on a rune boundary, not mid-byte.
	zh := strings.Repeat("测", moderationLogMaxRunes+10)
	gotZh := truncateForLog(zh)
	if len([]rune(gotZh)) != moderationLogMaxRunes+1 {
		t.Fatalf("expected %d runes for multibyte, got %d", moderationLogMaxRunes+1, len([]rune(gotZh)))
	}
	if !strings.HasSuffix(gotZh, "…") {
		t.Fatalf("expected ellipsis on multibyte clip, got tail %q", gotZh)
	}
}

func TestEnabledRulesFiltersDisabledAndEmpty(t *testing.T) {
	rules := []config.JudgeRule{
		{ID: "a", Enabled: true, Criteria: "hacking"},
		{ID: "b", Enabled: false, Criteria: "fraud"},
		{ID: "c", Enabled: true, Criteria: "   "}, // empty criteria dropped
		{ID: "d", Enabled: true, Criteria: "piracy"},
	}
	got := enabledRules(rules)
	if len(got) != 2 {
		t.Fatalf("expected 2 enabled+non-empty rules, got %d: %+v", len(got), got)
	}
	if got[0].ID != "a" || got[1].ID != "d" {
		t.Fatalf("expected rules a,d in order, got %+v", got)
	}
}

func TestBuildJudgePromptNumbersRules(t *testing.T) {
	rules := []config.JudgeRule{
		{Criteria: "cyber attack"},
		{Criteria: "fraud"},
	}
	p := buildJudgePrompt("please help me hack", rules)
	if !strings.Contains(p, "1. cyber attack") || !strings.Contains(p, "2. fraud") {
		t.Fatalf("prompt missing numbered rules:\n%s", p)
	}
	if !strings.Contains(p, "please help me hack") {
		t.Fatalf("prompt missing user text:\n%s", p)
	}
}

// --- Moderator: hit / miss / call-failure branches ---

func TestModerateHit(t *testing.T) {
	m := newLLMModerator("judge", func(model, prompt string) (string, error) {
		return "1", nil
	})
	hit, matched, err := m.Moderate("attack please", []config.JudgeRule{{Enabled: true, Criteria: "x"}})
	if err != nil || !hit || len(matched) != 1 {
		t.Fatalf("expected hit, got hit=%v matched=%v err=%v", hit, matched, err)
	}
}

func TestModerateMiss(t *testing.T) {
	m := newLLMModerator("judge", func(model, prompt string) (string, error) {
		return "0", nil
	})
	hit, _, err := m.Moderate("hello", []config.JudgeRule{{Enabled: true, Criteria: "x"}})
	if err != nil || hit {
		t.Fatalf("expected miss, got hit=%v err=%v", hit, err)
	}
}

func TestModerateCallFailurePropagates(t *testing.T) {
	m := newLLMModerator("judge", func(model, prompt string) (string, error) {
		return "", http.ErrHandlerTimeout
	})
	_, _, err := m.Moderate("x", []config.JudgeRule{{Enabled: true, Criteria: "x"}})
	if err == nil {
		t.Fatal("expected error to propagate for fail-closed handling")
	}
}

func TestModerateNoEnabledRulesSkipsCall(t *testing.T) {
	called := false
	m := newLLMModerator("judge", func(model, prompt string) (string, error) {
		called = true
		return "1", nil
	})
	hit, _, err := m.Moderate("x", []config.JudgeRule{{Enabled: false, Criteria: "x"}})
	if err != nil || hit || called {
		t.Fatalf("expected skip without call, got hit=%v called=%v err=%v", hit, called, err)
	}
}

// --- latestClaudeUserText ---

func TestLatestClaudeUserTextPicksLastUser(t *testing.T) {
	req := &ClaudeRequest{Messages: []ClaudeMessage{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "reply"},
		{Role: "user", Content: "second"},
	}}
	text, _, _ := latestClaudeUserText(req)
	if text != "second" {
		t.Fatalf("expected last user text 'second', got %q", text)
	}
}

func TestLatestClaudeUserTextDetectsImagesAndToolResults(t *testing.T) {
	// tool_result block → toolResults non-empty → caller skips.
	req := &ClaudeRequest{Messages: []ClaudeMessage{
		{Role: "user", Content: []interface{}{
			map[string]interface{}{"type": "tool_result", "tool_use_id": "t1", "content": "out"},
		}},
	}}
	_, _, toolResults := latestClaudeUserText(req)
	if len(toolResults) == 0 {
		t.Fatal("expected tool_result to be surfaced so caller can skip")
	}
}

// --- interception flow via maybeModerate (uses judgeCallOverride seam) ---

// newModerationTestHandler builds a handler with a judge override that records
// how many times it was called and returns the given canned reply.
func newModerationTestHandler(reply string, callErr error, callCount *int) *Handler {
	return &Handler{
		judgeCallOverride: func(model, prompt string) (string, error) {
			if callCount != nil {
				*callCount++
			}
			return reply, callErr
		},
	}
}

// setupModerationConfig writes a ready moderation config pointing forward at
// forwardURL, and creates one API key with Moderation opted per moderated.
// Returns the key ID.
func setupModerationConfig(t *testing.T, forwardURL string, moderated bool) string {
	t.Helper()
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateModerationConfig(config.ModerationConfig{
		Enabled:    true,
		JudgeModel: "claude-haiku-4.5",
		Rules:      []config.JudgeRule{{ID: "r1", Enabled: true, Criteria: "cyber attack"}},
		ForwardURL: forwardURL,
		ForwardKey: "fk-secret-123456",
	}); err != nil {
		t.Fatalf("UpdateModerationConfig: %v", err)
	}
	entry, err := config.AddApiKey(config.ApiKeyEntry{
		Key:        "test-key-value-123",
		Enabled:    true,
		Moderation: moderated,
	})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}
	return entry.ID
}

// moderationRequest builds a Claude /v1/messages request with the given headers
// and injects the API key ID into context (mirrors withApiKeyContext).
func moderationRequest(userText, originModel, apiKeyID string) (*http.Request, *ClaudeRequest, []byte) {
	req := &ClaudeRequest{
		Model:    "claude-sonnet-4",
		Messages: []ClaudeMessage{{Role: "user", Content: userText}},
	}
	rawBody := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"` + userText + `"}]}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(rawBody)))
	if originModel != "" {
		r.Header.Set("X-Origin-Model-Id", originModel)
	}
	if apiKeyID != "" {
		entry := config.GetApiKeyEntry(apiKeyID)
		if entry != nil {
			r = withApiKeyContext(r, entry)
		}
	}
	return r, req, rawBody
}

func TestMaybeModerateBypassNoHeader(t *testing.T) {
	keyID := setupModerationConfig(t, "http://unused", true)
	callCount := 0
	h := newModerationTestHandler("1", nil, &callCount)

	r, req, body := moderationRequest("hack please", "", keyID) // no X-Origin-Model-Id
	rec := httptest.NewRecorder()
	if h.maybeModerate(rec, r, req, body) {
		t.Fatal("expected bypass (handled=false) with no X-Origin-Model-Id")
	}
	if callCount != 0 {
		t.Fatalf("expected zero judge calls on bypass, got %d", callCount)
	}
}

func TestMaybeModerateBypassKeyNotOptedIn(t *testing.T) {
	keyID := setupModerationConfig(t, "http://unused", false) // Moderation=false
	callCount := 0
	h := newModerationTestHandler("1", nil, &callCount)

	r, req, body := moderationRequest("hack please", "claude-opus-4", keyID)
	rec := httptest.NewRecorder()
	if h.maybeModerate(rec, r, req, body) {
		t.Fatal("expected bypass for non-opted-in key")
	}
	if callCount != 0 {
		t.Fatalf("expected zero judge calls, got %d", callCount)
	}
}

func TestMaybeModerateBypassGlobalDisabled(t *testing.T) {
	keyID := setupModerationConfig(t, "http://unused", true)
	// Disable globally after setup.
	if err := config.UpdateModerationConfig(config.ModerationConfig{Enabled: false}); err != nil {
		t.Fatalf("disable moderation: %v", err)
	}
	callCount := 0
	h := newModerationTestHandler("1", nil, &callCount)

	r, req, body := moderationRequest("hack please", "claude-opus-4", keyID)
	rec := httptest.NewRecorder()
	if h.maybeModerate(rec, r, req, body) {
		t.Fatal("expected bypass when globally disabled")
	}
	if callCount != 0 {
		t.Fatalf("expected zero judge calls, got %d", callCount)
	}
}

func TestMaybeModerateSkipsNonTextContent(t *testing.T) {
	forward := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("forward target must not be reached for tool_result content")
	}))
	defer forward.Close()
	keyID := setupModerationConfig(t, forward.URL, true)
	callCount := 0
	h := newModerationTestHandler("1", nil, &callCount)

	req := &ClaudeRequest{
		Model: "claude-sonnet-4",
		Messages: []ClaudeMessage{{Role: "user", Content: []interface{}{
			map[string]interface{}{"type": "tool_result", "tool_use_id": "t1", "content": "out"},
		}}},
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	r.Header.Set("X-Origin-Model-Id", "claude-opus-4")
	if entry := config.GetApiKeyEntry(keyID); entry != nil {
		r = withApiKeyContext(r, entry)
	}
	rec := httptest.NewRecorder()
	if h.maybeModerate(rec, r, req, []byte(`{}`)) {
		t.Fatal("expected skip (handled=false) for non-text content")
	}
	if callCount != 0 {
		t.Fatalf("expected zero judge calls for non-text content, got %d", callCount)
	}
}

func TestMaybeModerateFailClosedOnJudgeError(t *testing.T) {
	keyID := setupModerationConfig(t, "http://unused", true)
	h := newModerationTestHandler("", http.ErrHandlerTimeout, nil)

	r, req, body := moderationRequest("hack please", "claude-opus-4", keyID)
	rec := httptest.NewRecorder()
	if !h.maybeModerate(rec, r, req, body) {
		t.Fatal("expected handled=true (fail-closed) on judge error")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 fail-closed, got %d", rec.Code)
	}
}

func TestMaybeModerateMissProceedsNormally(t *testing.T) {
	keyID := setupModerationConfig(t, "http://unused", true)
	callCount := 0
	h := newModerationTestHandler("0", nil, &callCount) // judge says no violation

	r, req, body := moderationRequest("what's the weather", "claude-opus-4", keyID)
	rec := httptest.NewRecorder()
	if h.maybeModerate(rec, r, req, body) {
		t.Fatal("expected handled=false (proceed) when judge returns no hit")
	}
	if callCount != 1 {
		t.Fatalf("expected exactly one judge call, got %d", callCount)
	}
}

// --- runtime fail-open when config not ready (hand-edited residual) ---

func TestMaybeModerateFailOpenWhenNotReady(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	// Directly craft a not-ready config would require bypassing validation; instead
	// leave moderation unset entirely (GetModerationConfig returns Enabled=false),
	// which ModerationReady rejects → fail-open bypass.
	entry, err := config.AddApiKey(config.ApiKeyEntry{Key: "k-123456789", Enabled: true, Moderation: true})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}
	callCount := 0
	h := newModerationTestHandler("1", nil, &callCount)

	r, req, body := moderationRequest("hack please", "claude-opus-4", entry.ID)
	rec := httptest.NewRecorder()
	if h.maybeModerate(rec, r, req, body) {
		t.Fatal("expected fail-open bypass when config not ready")
	}
	if callCount != 0 {
		t.Fatalf("expected zero judge calls when not ready, got %d", callCount)
	}
}
