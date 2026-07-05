package proxy

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// Moderator classifies whether a piece of user text requests disallowed
// behavior, per a set of enabled rules. The concrete llmModerator uses a cheap
// judge model in the Kiro pool; the interface is the Phase-2 extension point for
// alternative engines. A pluggable seam, not a promise of more engines now.
type Moderator interface {
	// Moderate returns hit=true when the text matches at least one enabled rule.
	// matched holds the 1-based rule numbers the judge flagged (for logging). A
	// non-nil error means the judge call itself failed — callers MUST treat this
	// as fail-closed (reject the request), never as "no hit".
	Moderate(userText string, rules []config.JudgeRule) (hit bool, matched []int, err error)
}

// judgeCaller performs a single-shot text completion against the judge model and
// returns the assistant's raw text. Abstracted so tests can inject a
// deterministic verdict without touching the Kiro pool. The real implementation
// (handler-backed) mirrors the internal single-shot skeleton at
// handler.go apiTestAccount.
type judgeCaller func(model, prompt string) (string, error)

// llmModerator is the default LLM-judge Moderator. It renders all enabled rules
// into one numbered prompt and asks the model to reply with only the matched
// rule numbers (or 0 for none), then parses the reply leniently.
type llmModerator struct {
	model string
	call  judgeCaller
}

func newLLMModerator(model string, call judgeCaller) *llmModerator {
	return &llmModerator{model: model, call: call}
}

// enabledRules filters to the rules that are switched on and carry a non-empty
// criteria. Returns the surviving rules in order; the returned slice's index+1
// is the number shown to the judge.
func enabledRules(rules []config.JudgeRule) []config.JudgeRule {
	out := make([]config.JudgeRule, 0, len(rules))
	for _, r := range rules {
		if r.Enabled && strings.TrimSpace(r.Criteria) != "" {
			out = append(out, r)
		}
	}
	return out
}

// buildJudgePrompt renders the classification instruction. Rules are numbered
// 1..N in the order given. The model is told to answer with only the matched
// numbers (comma-separated) or 0 when nothing matches.
func buildJudgePrompt(userText string, rules []config.JudgeRule) string {
	var b strings.Builder
	b.WriteString("You are a strict intent classifier. Given a user message and a numbered list of rules, ")
	b.WriteString("decide which rules (if any) the message VIOLATES by requesting the described disallowed behavior.\n\n")
	b.WriteString("Rules:\n")
	for i, r := range rules {
		fmt.Fprintf(&b, "%d. %s\n", i+1, strings.TrimSpace(r.Criteria))
	}
	b.WriteString("\nRespond with ONLY the numbers of the rules the message violates, comma-separated ")
	b.WriteString("(e.g. \"1,3\"). If it violates none, respond with exactly \"0\". ")
	b.WriteString("Do not explain. Do not add any other text.\n\n")
	b.WriteString("User message:\n")
	b.WriteString("<<<\n")
	b.WriteString(userText)
	b.WriteString("\n>>>\n")
	return b.String()
}

// numberPattern extracts standalone integers from the judge reply. Lenient by
// design: the model may wrap the answer in noise ("Rules 1 and 3 apply.") and we
// still recover the numbers.
var numberPattern = regexp.MustCompile(`\d+`)

// parseVerdict interprets the judge reply against the number of rules presented.
// Any number in [1..ruleCount] counts as a hit for that rule; 0 (and only 0) is
// the explicit "no violation" signal. Numbers out of range are ignored (model
// noise). hit is true when at least one in-range rule number is present.
func parseVerdict(reply string, ruleCount int) (hit bool, matched []int) {
	found := numberPattern.FindAllString(reply, -1)
	seen := make(map[int]bool)
	for _, s := range found {
		n, err := strconv.Atoi(s)
		if err != nil {
			continue
		}
		if n >= 1 && n <= ruleCount && !seen[n] {
			seen[n] = true
			matched = append(matched, n)
		}
	}
	return len(matched) > 0, matched
}

// moderationLogMaxRunes caps how much user text / judge reply is written to the
// observation debug log, keeping full conversations out of the log while leaving
// enough to eyeball whether a verdict was reasonable.
const moderationLogMaxRunes = 200

// truncateForLog returns s trimmed and clipped to moderationLogMaxRunes runes,
// with an ellipsis marker when clipped. Rune-based so multibyte (Chinese) text
// isn't cut mid-character.
func truncateForLog(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= moderationLogMaxRunes {
		return s
	}
	return string(r[:moderationLogMaxRunes]) + "…"
}

// Moderate implements Moderator. With no enabled rules there is nothing to flag,
// so it returns (false, nil, nil) without calling the judge. A judge call error
// is propagated so the caller can fail-closed.
func (m *llmModerator) Moderate(userText string, rules []config.JudgeRule) (bool, []int, error) {
	active := enabledRules(rules)
	if len(active) == 0 {
		return false, nil, nil
	}
	prompt := buildJudgePrompt(userText, active)
	reply, err := m.call(m.model, prompt)
	if err != nil {
		return false, nil, fmt.Errorf("judge call failed: %w", err)
	}
	hit, matched := parseVerdict(reply, len(active))
	// Observation log (debug-gated): one line per judged request with the (clipped)
	// user text, the raw judge reply, and the parsed verdict. Lets operators
	// eyeball hit/miss accuracy during a trial period without a standing info-level
	// stream that would echo conversation content into normal logs. Turn on with
	// LOG_LEVEL=debug; turn off by reverting to info — no code change needed.
	logger.Debugf("moderation judge: verdict hit=%v matched=%v | reply=%q | userText=%q",
		hit, matched, truncateForLog(reply), truncateForLog(userText))
	return hit, matched, nil
}

// kiroJudgeCall is the production judgeCaller: it runs a single-shot text
// completion against the given model using the Kiro account pool, mirroring the
// internal single-shot skeleton at handler.go apiTestAccount (build payload →
// collect text via callback → CallKiroAPI). Any account/upstream failure is
// returned as an error so the caller can fail-closed. The judge call itself is
// NOT metered into usage.
func (h *Handler) kiroJudgeCall(model, prompt string) (string, error) {
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(model, thinkingCfg.Suffix)

	openaiReq := &OpenAIRequest{
		Model:     actualModel,
		Messages:  []OpenAIMessage{{Role: "user", Content: prompt}},
		MaxTokens: 16, // verdict is just rule numbers or "0"
		Stream:    false,
	}
	kiroPayload := OpenAIToKiro(openaiReq, thinking)

	excluded := make(map[string]bool)
	var lastErr error
	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account := h.pool.GetNextForModelExcluding(actualModel, excluded)
		if account == nil {
			break
		}
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		var content string
		var callErr error
		var judgeCredits float64
		callback := &KiroStreamCallback{
			OnText:         func(text string, isThinking bool) { content += text },
			OnToolUse:      func(tu KiroToolUse) {},
			OnComplete:     func(inTok, outTok int) {},
			OnError:        func(err error) { callErr = err },
			OnCredits:      func(c float64) { judgeCredits = c },
			OnContextUsage: func(pct float64) {},
		}

		if err := CallKiroAPI(account, kiroPayload, callback); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}
		if callErr != nil {
			lastErr = callErr
			excluded[account.ID] = true
			h.handleAccountFailure(account, callErr)
			continue
		}
		// Track the judge call's credit cost against the day's moderation total.
		// The judge is not metered as a client request (no usage/output billing),
		// but it does spend the account's upstream credits, so surface that cost
		// separately for accounting. Only the successful account is charged.
		if judgeCredits > 0 {
			h.pool.UpdateStats(account.ID, 0, judgeCredits)
			config.RecordDailyModerationCredits(account.ID, judgeCredits)
		}
		return content, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no account available for judge model %q", actualModel)
	}
	return "", lastErr
}

// maybeModerate implements the §3 interception. It runs BEFORE the normal Claude
// flow's thinking/cacheProfile/ClaudeToKiro work, so a hit-forward request never
// wastes that computation. Return value:
//   - handled=true  → the request is fully dealt with here (forwarded, or failed
//     closed); the caller must return immediately without touching w further.
//   - handled=false → moderation did not apply / the message passed; the caller
//     proceeds with the normal Kiro flow.
//
// Fail semantics: config not ready → handled=false (fail-open, §5.1). Judge call
// error → request failed with 503, handled=true (fail-closed, §3 step 4).
func (h *Handler) maybeModerate(w http.ResponseWriter, r *http.Request, req *ClaudeRequest, rawBody []byte) (handled bool) {
	// Step 1: X-Origin-Model-Id absent → total bypass, zero cost.
	originModel := strings.TrimSpace(r.Header.Get("X-Origin-Model-Id"))
	if originModel == "" {
		// Observation log (debug-gated): record the bypass so downstream integrators
		// can confirm why moderation didn't run (missing control header). Shares the
		// "moderation judge" prefix so it shows up in the same grep as verdicts.
		logger.Debugf("moderation judge: bypass, no X-Origin-Model-Id header (moderation not applied)")
		return false
	}

	// Step 2a: global gateway must be enabled AND fully configured (runtime
	// fail-open guard, defense-in-depth against a hand-edited config.json).
	mc := config.GetModerationConfig()
	if !mc.ModerationReady() {
		return false
	}

	// Step 2b: the request's API key must be opted in. Context holds only the key
	// ID (proxy/auth.go), so look the entry up. No key / unknown key → not opted in.
	apiKeyID := apiKeyIDFromContext(r.Context())
	if apiKeyID == "" {
		return false
	}
	entry := config.GetApiKeyEntry(apiKeyID)
	if entry == nil || !entry.Moderation {
		return false
	}

	// Step 3: extract the LATEST user message's plain text. Walk messages from the
	// end to find the last role=="user"; feed only that single message's Content
	// to extractClaudeUserContent (it takes one content value, not the array).
	userText, images, toolResults := latestClaudeUserText(req)
	// Non-text content (images / tool_result) or empty text → skip judging.
	if userText == "" || len(images) > 0 || len(toolResults) > 0 {
		return false
	}

	// Strip client-injected context tags (file contents, system reminders, etc.)
	// before judging — their contents are ambient context, not user intent, and
	// caused many false positives. Only the JUDGE sees the stripped text; the
	// forwarded body and the logged Input keep the full original message. If
	// nothing but injected context remains (no actual user-typed text), there is
	// no intent to classify → skip judging and take the normal flow.
	judgeText := stripInjectedContext(userText)
	if judgeText == "" {
		return false
	}

	// Step 4: judge.
	call := h.kiroJudgeCall
	if h.judgeCallOverride != nil {
		call = h.judgeCallOverride
	}
	moderator := newLLMModerator(mc.JudgeModel, call)
	hit, matched, err := moderator.Moderate(judgeText, mc.Rules)
	if err != nil {
		// fail-closed: judge unavailable → reject, client retries.
		logger.Errorf("moderation: judge call failed, failing closed: %v", err)
		h.sendClaudeError(w, 503, "api_error", "Moderation service temporarily unavailable")
		return true
	}
	if !hit {
		return false // passed classification → normal flow
	}

	logger.Infof("moderation: hit (rules=%v), forwarding to configured target", matched)
	// Record the forwarded request in the "moderation" log ring so operators can
	// review hit content in the admin panel (Logs → 仅审核), independent of the
	// debug-gated stdout logs. Input holds the exact user message that was
	// forwarded (latest user turn); Model is the origin model it was forwarded as.
	h.appendRequestLog(RequestLog{
		Time:         time.Now().Unix(),
		Endpoint:     "claude",
		Model:        originModel,
		Status:       "moderation",
		Input:        userText,
		MatchedRules: matched,
	})
	h.forwardModeratedRequest(w, r, rawBody, originModel, mc)
	return true
}

// moderationStripTags are client-injected context wrappers (Claude Code and
// similar) that carry ambient context — file contents, system reminders, tool
// results, environment details — NOT the user's actual intent. Their contents
// are removed before the text is shown to the judge: security/attack vocabulary
// living inside injected file contents or reminders was causing large numbers of
// false positives. This stripping affects ONLY the text handed to the judge; the
// forwarded body and the logged Input keep the full original message.
var moderationStripTags = []string{
	"system-reminder",
	"system_reminder",
	"file_contents",
	"document",
	"function_results",
	"function_calls",
	"environment_details",
}

// moderationStripRes is one compiled regex per tag: <tag ...>...</tag>,
// case-insensitive (?i), dot-matches-newline (?s), non-greedy. RE2 has no
// backreferences, so we compile one regex per tag rather than a single
// alternation closed with \1.
var moderationStripRes = func() []*regexp.Regexp {
	res := make([]*regexp.Regexp, 0, len(moderationStripTags))
	for _, tag := range moderationStripTags {
		q := regexp.QuoteMeta(tag)
		res = append(res, regexp.MustCompile(`(?is)<`+q+`\b[^>]*>.*?</`+q+`>`))
	}
	return res
}()

// stripInjectedContext removes client-injected context tag blocks (see
// moderationStripTags) from text before it is classified by the judge, then
// trims surrounding whitespace. Returns the remaining "bare" user text.
func stripInjectedContext(text string) string {
	for _, re := range moderationStripRes {
		text = re.ReplaceAllString(text, "")
	}
	return strings.TrimSpace(text)
}

// latestClaudeUserText finds the last message with role=="user" and extracts its
// plain text (and any images / tool_results, so the caller can decide to skip).
// Returns empty text when there is no user message.
func latestClaudeUserText(req *ClaudeRequest) (string, []KiroImage, []KiroToolResult) {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			text, images, toolResults := extractClaudeUserContent(req.Messages[i].Content)
			return strings.TrimSpace(text), images, toolResults
		}
	}
	return "", nil, nil
}
