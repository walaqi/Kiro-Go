package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestClaudeToKiroTruncatesOversizedHistory builds a conversation whose history
// far exceeds the upstream input limit and verifies the converted payload is
// trimmed below maxPayloadBytes, that a truncation placeholder is inserted, and
// that the current message is preserved.
func TestClaudeToKiroTruncatesOversizedHistory(t *testing.T) {
	// ~2KB chunk repeated across many turns to blow past the byte limit.
	big := strings.Repeat("lorem ipsum dolor sit amet ", 80) // ~2.1KB

	msgs := []ClaudeMessage{
		{Role: "user", Content: "start the long task"},
	}
	for i := 0; i < 800; i++ {
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: "step result: " + big},
			ClaudeMessage{Role: "user", Content: "next: " + big},
		)
	}
	msgs = append(msgs, ClaudeMessage{Role: "user", Content: "FINAL: summarize everything above"})

	req := &ClaudeRequest{
		Model:    "claude-opus-4.8",
		System:   "You are a helpful assistant.",
		Messages: msgs,
	}

	payload := ClaudeToKiro(req, false)

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if len(raw) > maxPayloadBytes {
		t.Fatalf("payload size %d exceeds limit %d after truncation", len(raw), maxPayloadBytes)
	}

	// The current message must be preserved.
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if !strings.Contains(cur.Content, "FINAL: summarize everything above") {
		t.Fatalf("current message lost after truncation, got %q", cur.Content[:min(80, len(cur.Content))])
	}

	// A truncation placeholder must be present in history.
	foundPlaceholder := false
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage != nil && strings.Contains(h.UserInputMessage.Content, "truncated to fit") {
			foundPlaceholder = true
			break
		}
	}
	if !foundPlaceholder {
		t.Fatalf("expected a truncation placeholder in history")
	}

	// System priming should still be at the front.
	if len(payload.ConversationState.History) < 2 {
		t.Fatalf("expected priming retained, history too short")
	}
	primingUser := payload.ConversationState.History[0].UserInputMessage
	if primingUser == nil || !strings.Contains(primingUser.Content, "helpful assistant") {
		t.Fatalf("expected system priming retained at front")
	}
}

// TestClaudeToKiroSmallPayloadNotTruncated ensures normal-sized conversations
// are left untouched (no placeholder inserted).
func TestClaudeToKiroSmallPayloadNotTruncated(t *testing.T) {
	req := &ClaudeRequest{
		Model:  "claude-opus-4.8",
		System: "You are helpful.",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi"},
			{Role: "user", Content: "how are you?"},
		},
	}
	payload := ClaudeToKiro(req, false)
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage != nil && strings.Contains(h.UserInputMessage.Content, "truncated to fit") {
			t.Fatalf("small payload should not be truncated")
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestClaudeToKiroTruncationDoesNotOrphanToolPairs builds an oversized history
// made entirely of structured tool-call pairs (assistant toolUse answered by the
// next user toolResult). Byte-size truncation drops the oldest turns, which can
// sever a pair — dropping the assistant call while keeping the answering user
// result. Such an orphaned toolResult makes the upstream reject the request with
// "Improperly formed request." (HTTP 400). This test asserts the converted
// payload contains no orphaned structured toolResults after truncation, and that
// user/assistant turns still strictly alternate (no consecutive user turns from
// a standalone truncation placeholder).
func TestClaudeToKiroTruncationDoesNotOrphanToolPairs(t *testing.T) {
	t.Setenv("KIRO_PRESERVE_TOOL_HISTORY", "on")

	big := strings.Repeat("tool output data block ", 350) // ~8KB per result

	msgs := []ClaudeMessage{{Role: "user", Content: "begin"}}
	// Enough pairs to blow well past maxPayloadBytes (1.5MB) so truncation fires
	// and must cut through a pair somewhere.
	for i := 0; i < 400; i++ {
		id := "call_" + strings.Repeat("x", 3) + string(rune('A'+i%26)) + string(rune('0'+i%10)) + "_" + string(rune('a'+i%26))
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "tool_use", "id": id, "name": "fsRead", "input": map[string]interface{}{"path": "/f"}},
			}},
			ClaudeMessage{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": id, "content": big},
			}},
		)
	}
	msgs = append(msgs, ClaudeMessage{Role: "user", Content: "FINAL: summarize"})

	payload := ClaudeToKiro(&ClaudeRequest{Model: "claude-opus-4.8", Messages: msgs}, false)

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if len(raw) > maxPayloadBytes {
		t.Fatalf("payload %d exceeds limit %d after truncation", len(raw), maxPayloadBytes)
	}

	hist := payload.ConversationState.History

	// Collect every tool-use ID emitted by an assistant turn in the surviving
	// history. The current message's toolResults (if any) answer the final
	// history assistant turn, but here the current message is a plain user turn.
	emitted := map[string]bool{}
	for _, h := range hist {
		if a := h.AssistantResponseMessage; a != nil {
			for _, tu := range a.ToolUses {
				emitted[tu.ToolUseID] = true
			}
		}
	}
	// No surviving structured toolResult may reference a tool-use ID that no
	// surviving assistant turn emitted — that is exactly the orphan that triggers
	// the upstream 400.
	for i, h := range hist {
		if u := h.UserInputMessage; u != nil && u.UserInputMessageContext != nil {
			for _, tr := range u.UserInputMessageContext.ToolResults {
				if !emitted[tr.ToolUseID] {
					t.Fatalf("history[%d] carries orphaned structured toolResult %q (upstream rejects)", i, tr.ToolUseID)
				}
			}
		}
	}

	// Strict alternation: no two consecutive user turns. A standalone truncation
	// placeholder user turn followed by the tail's leading user turn would violate
	// this; the placeholder text must be merged into the first retained user turn.
	for i := 1; i < len(hist); i++ {
		if hist[i-1].UserInputMessage != nil && hist[i].UserInputMessage != nil {
			t.Fatalf("consecutive user turns at history[%d],[%d] break alternation", i-1, i)
		}
	}

	// The truncation notice must still be present somewhere in history.
	foundNotice := false
	for _, h := range hist {
		if h.UserInputMessage != nil && strings.Contains(h.UserInputMessage.Content, "truncated to fit") {
			foundNotice = true
			break
		}
	}
	if !foundNotice {
		t.Fatalf("expected truncation notice in history after oversized truncation")
	}
}
