package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"kiro-go/config"
	"kiro-go/logger"
)

// forwardClientCache holds one Timeout=0 http.Client per outbound proxy URL.
// Unlike the auth REST client (30s timeout), forward clients use Timeout=0 so
// streamed SSE responses can run for minutes; the request lifetime is bounded by
// the inbound request's context instead (see forwardModeratedRequest).
//
// Keyed by proxy URL (empty string = direct) so a proxy config hot-update is
// picked up on the next request: getForwardClient re-reads config.GetProxyURL()
// each call and builds/reuses the client for the current proxy, rather than
// freezing the first-seen proxy in a sync.Once singleton.
var forwardClientCache sync.Map // map[string]*http.Client

func getForwardClient() *http.Client {
	proxyURL := config.GetProxyURL()
	if cached, ok := forwardClientCache.Load(proxyURL); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		Timeout:   0, // no overall deadline; SSE streams may be long-lived
		Transport: buildKiroTransport(proxyURL),
	}
	actual, _ := forwardClientCache.LoadOrStore(proxyURL, client)
	return actual.(*http.Client)
}

// forwardBodyParamKeys are the top-level generation params copied verbatim from
// the downstream request into the forwarded body. Everything else — conversation
// history, system prompt, tools, tool_choice, thinking, and any custom fields —
// is intentionally dropped: only the latest user message is forwarded, to
// minimize how much conversation content leaves the service on a hit.
//
// "stream" is included so the target responds in the same shape (SSE vs JSON)
// the downstream client expects, since we pass the target's response through
// unchanged.
var forwardBodyParamKeys = []string{
	"max_tokens", "stream", "temperature", "top_p", "top_k", "stop_sequences", "metadata",
}

// defaultForwardMaxTokens is injected into the forwarded body when the downstream
// request omitted max_tokens. Anthropic's Messages API requires the field, so a
// standard-compliant forward target would 400 without it. Conservative default.
const defaultForwardMaxTokens = 1024

// rewriteForwardBody rebuilds the forward request body from the downstream
// request. Behavior depends on fullContent:
//
//   - false (default, data-minimized): keep only the latest user message
//     (history / system / tools dropped) plus a whitelist of generation params.
//     The target sees just the current user turn, minimizing conversation
//     content that leaves the service on a hit.
//   - true: forward the FULL original body verbatim — history, system, tools,
//     and any custom fields all preserved — swapping ONLY the model. Controlled
//     by the admin "forward full content" toggle (ModerationConfig.ForwardFullContent).
//
// Either way max_tokens is ensured (Anthropic requires it). In minimized mode the
// latest user message's content is preserved exactly as sent; moderation only
// reaches this path when that message is pure text, so no non-text content is
// involved.
func rewriteForwardBody(rawBody []byte, originModel string, fullContent bool) ([]byte, error) {
	var body map[string]interface{}
	if err := json.Unmarshal(rawBody, &body); err != nil {
		return nil, fmt.Errorf("forward: cannot parse request body: %w", err)
	}

	var out map[string]interface{}
	if fullContent {
		// Full passthrough: keep every field, swap only the model.
		out = body
		out["model"] = originModel
	} else {
		lastUser, ok := lastUserMessageFromBody(body)
		if !ok {
			return nil, fmt.Errorf("forward: no user message to forward")
		}
		out = map[string]interface{}{
			"model":    originModel,
			"messages": []interface{}{lastUser},
		}
		for _, k := range forwardBodyParamKeys {
			if v, ok := body[k]; ok {
				out[k] = v
			}
		}
	}

	// max_tokens is REQUIRED by the Anthropic Messages API. Kiro-Go's own inbound
	// validation does not enforce it, so a downstream request may omit it; forward
	// it as-is when present, otherwise inject a conservative default so a standard
	// Anthropic-compatible target doesn't reject the forwarded request with a 400.
	if _, ok := out["max_tokens"]; !ok {
		out["max_tokens"] = defaultForwardMaxTokens
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("forward: cannot re-marshal request body: %w", err)
	}
	return encoded, nil
}

// lastUserMessageFromBody returns the last messages[] entry whose role=="user",
// as the raw map from the parsed body so its content structure is preserved
// verbatim. Returns ok=false when there is no messages array or no user message.
func lastUserMessageFromBody(body map[string]interface{}) (map[string]interface{}, bool) {
	raw, ok := body["messages"].([]interface{})
	if !ok {
		return nil, false
	}
	for i := len(raw) - 1; i >= 0; i-- {
		msg, ok := raw[i].(map[string]interface{})
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "user" {
			return msg, true
		}
	}
	return nil, false
}

// forwardModeratedRequest forwards a moderated-hit request to the configured
// ForwardURL and streams the target's response back to the downstream client.
//
// Security-critical header handling: a BRAND-NEW outbound request is built and
// only whitelisted headers are set — the downstream's Authorization, X-Api-Key,
// and the X-Origin-Model-Id control header are NEVER propagated. We build up
// from nothing rather than copying r.Header and deleting, so no sensitive header
// can leak by omission.
//
// The outbound request derives from r.Context(): when the downstream client
// disconnects, the context cancels and the forward connection is torn down,
// preventing a long-lived SSE forward from spinning after the client is gone.
func (h *Handler) forwardModeratedRequest(w http.ResponseWriter, r *http.Request, rawBody []byte, originModel string, mc config.ModerationConfig) {
	newBody, err := rewriteForwardBody(rawBody, originModel, mc.ForwardFullContent)
	if err != nil {
		logger.Errorf("moderation forward: body rewrite failed: %v", err)
		h.sendClaudeError(w, 400, "invalid_request_error", "Failed to prepare forwarded request")
		return
	}

	// Observation log (debug-gated): the exact body about to be forwarded, printed
	// in full so operators can confirm it carries NO filter injection / system-
	// prompt rewrite (those only happen inside ClaudeToKiro, which the forward path
	// never touches — only "model" is swapped). Full text, not clipped, since the
	// system field is what we're verifying and it can sit anywhere in the body.
	// Turn on with LOG_LEVEL=debug; revert to info to stop. May be large.
	logger.Debugf("moderation forward: POST %s (model→%q), body=%s", mc.ForwardURL, originModel, string(newBody))

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, mc.ForwardURL, bytes.NewReader(newBody))
	if err != nil {
		logger.Errorf("moderation forward: build request failed: %v", err)
		h.sendClaudeError(w, 500, "api_error", "Failed to build forwarded request")
		return
	}
	// Whitelist-only headers. Do NOT copy r.Header — the downstream's own
	// Authorization / x-api-key and the X-Origin-Model-Id control header must not
	// leak to the target. Only the forward key is sent, as a Bearer token. (A
	// single auth scheme avoids double-auth rejections from strict OpenAI/Claude-
	// compatible gateways.)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+mc.ForwardKey)

	resp, err := getForwardClient().Do(req)
	if err != nil {
		logger.Errorf("moderation forward: request to target failed: %v", err)
		h.sendClaudeError(w, 502, "api_error", "Forward target unreachable")
		return
	}
	defer resp.Body.Close()

	// Pass through the target's real response shape: status + Content-Type (so the
	// downstream sees SSE vs JSON), plus a couple of streaming-relevant headers.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "" {
		w.Header().Set("Cache-Control", cc)
	}
	w.WriteHeader(resp.StatusCode)

	// Stream the body back, flushing per chunk so SSE stays real-time. A plain
	// io.Copy would buffer and defeat streaming.
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return // downstream gone
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				logger.Warnf("moderation forward: read from target interrupted: %v", readErr)
			}
			return
		}
	}
}
