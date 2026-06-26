package proxy

import (
	"fmt"
	"strings"
	"testing"
)

// The default ("preserve") strategy keeps structured tool calls/results in
// history intact whenever they form a complete pair, matching what the real
// Kiro IDE client sends upstream. These tests pin the strategy ON explicitly so
// they are independent of the package default, and cover the three behaviors
// that distinguish preserve from the flatten fallback:
//
//   1. Complete pairs stay structured (no "Tool results:" narration seeded).
//   2. An orphaned assistant toolUse (no answering results) is stripped.
//   3. An orphaned user toolResult (no preceding call) is narrated to text so
//      its data survives.

// TestClaudeToKiroPreservesStructuredToolHistory verifies that a multi-cycle
// tool conversation keeps every assistant toolUse and user toolResult as
// structured data, and seeds no "Tool results:" narration into history.
func TestClaudeToKiroPreservesStructuredToolHistory(t *testing.T) {
	t.Setenv("KIRO_PRESERVE_TOOL_HISTORY", "on")

	msgs := []ClaudeMessage{{Role: "user", Content: "do a multi-step task"}}
	for i := 0; i < 6; i++ {
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "text", "text": fmt.Sprintf("step %d", i)},
				map[string]interface{}{"type": "tool_use", "id": fmt.Sprintf("t%d", i), "name": "exec_command", "input": map[string]interface{}{"cmd": fmt.Sprintf("c%d", i)}},
			}},
			ClaudeMessage{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": fmt.Sprintf("t%d", i), "content": fmt.Sprintf("OUT_%d", i)},
			}},
		)
	}
	// Final plain-text user instruction so the last tool cycle lives entirely in
	// history (its toolResults are not the current message).
	msgs = append(msgs, ClaudeMessage{Role: "user", Content: "now summarize"})

	payload := ClaudeToKiro(&ClaudeRequest{
		Model:    "claude-opus-4.8",
		Messages: msgs,
		Tools: []ClaudeTool{
			{Name: "exec_command", Description: "run a command", InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"cmd": map[string]interface{}{"type": "string"}}}},
		},
	}, false)

	var structuredCalls, structuredResults, narratedTurns int
	for _, h := range payload.ConversationState.History {
		if a := h.AssistantResponseMessage; a != nil {
			structuredCalls += len(a.ToolUses)
		}
		if u := h.UserInputMessage; u != nil {
			if u.UserInputMessageContext != nil {
				structuredResults += len(u.UserInputMessageContext.ToolResults)
			}
			if strings.Contains(u.Content, toolResultsContinuationPrefix) {
				narratedTurns++
			}
		}
	}

	if structuredCalls != 6 {
		t.Fatalf("expected 6 structured toolUses preserved, got %d", structuredCalls)
	}
	if structuredResults != 6 {
		t.Fatalf("expected 6 structured toolResults preserved, got %d", structuredResults)
	}
	if narratedTurns != 0 {
		t.Fatalf("expected no %q narration seeded into history, got %d turns", toolResultsContinuationPrefix, narratedTurns)
	}
}

// TestPreserveStripsOrphanedAssistantToolUse covers an assistant tool call whose
// answering result turn was dropped (e.g. context compaction). The orphaned
// toolUse must be stripped (upstream rejects unpaired structure) while the
// assistant's natural-language text survives, and no tool-call narration is
// written into the assistant turn.
func TestPreserveStripsOrphanedAssistantToolUse(t *testing.T) {
	t.Setenv("KIRO_PRESERVE_TOOL_HISTORY", "on")

	msgs := []ClaudeMessage{
		{Role: "user", Content: "start"},
		// Orphaned call: assistant invokes a tool, but no tool_result follows.
		{Role: "assistant", Content: []interface{}{
			map[string]interface{}{"type": "text", "text": "let me check the file"},
			map[string]interface{}{"type": "tool_use", "id": "orphan1", "name": "fsRead", "input": map[string]interface{}{"path": "/x"}},
		}},
		// A plain user turn follows (no matching tool_result) — the compaction cut.
		{Role: "user", Content: "actually, never mind, just answer directly"},
	}

	payload := ClaudeToKiro(&ClaudeRequest{Model: "claude-opus-4.8", Messages: msgs}, false)

	for i, h := range payload.ConversationState.History {
		if a := h.AssistantResponseMessage; a != nil {
			if len(a.ToolUses) > 0 {
				t.Fatalf("history[%d] orphaned toolUse not stripped: %d remain", i, len(a.ToolUses))
			}
			if strings.Contains(a.Content, "[Called tool") || strings.Contains(a.Content, "fsRead") {
				t.Fatalf("history[%d] assistant content carries mimicable tool narration: %q", i, a.Content)
			}
		}
	}

	// The assistant's prose must survive.
	var combined strings.Builder
	for _, h := range payload.ConversationState.History {
		if h.AssistantResponseMessage != nil {
			combined.WriteString(h.AssistantResponseMessage.Content)
			combined.WriteString("\n")
		}
	}
	if !strings.Contains(combined.String(), "let me check the file") {
		t.Fatalf("expected orphaned-call assistant prose to survive, got:\n%s", combined.String())
	}
}

// TestPreserveNarratesOrphanedToolResult covers a user toolResult that has no
// preceding assistant toolUse (e.g. the call turn was dropped by compaction).
// Its structured form is rejected upstream, so it must be narrated to text to
// preserve the data, attributed to its tool when the name is known.
func TestPreserveNarratesOrphanedToolResult(t *testing.T) {
	t.Setenv("KIRO_PRESERVE_TOOL_HISTORY", "on")

	// First a complete pair (so "fsRead" is a known tool name), then an orphaned
	// result whose call turn is absent.
	msgs := []ClaudeMessage{
		{Role: "user", Content: "start"},
		{Role: "assistant", Content: []interface{}{
			map[string]interface{}{"type": "tool_use", "id": "paired", "name": "fsRead", "input": map[string]interface{}{"path": "/a"}},
		}},
		{Role: "user", Content: []interface{}{
			map[string]interface{}{"type": "tool_result", "tool_use_id": "paired", "content": "PAIRED_OUTPUT"},
		}},
		// Orphaned result: no assistant toolUse precedes this one.
		{Role: "user", Content: []interface{}{
			map[string]interface{}{"type": "tool_result", "tool_use_id": "ghost", "content": "ORPHAN_DATA_XYZ"},
		}},
		{Role: "user", Content: "summarize"},
	}

	payload := ClaudeToKiro(&ClaudeRequest{Model: "claude-opus-4.8", Messages: msgs}, false)

	// The orphaned result's data must survive as text somewhere in history.
	var combined strings.Builder
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage != nil {
			combined.WriteString(h.UserInputMessage.Content)
			combined.WriteString("\n")
		}
	}
	if !strings.Contains(combined.String(), "ORPHAN_DATA_XYZ") {
		t.Fatalf("expected orphaned tool-result data narrated to text, got:\n%s", combined.String())
	}
}

// TestStubToolConfigInjectedWhenNoToolsInRequest verifies that when the current
// request carries no tool definitions but history contains structured tool blocks,
// stub toolConfig is injected into UserInputMessageContext so Bedrock's
// TOOL_CONFIG_MISSING validation passes without flattening history.
func TestStubToolConfigInjectedWhenNoToolsInRequest(t *testing.T) {
	t.Setenv("KIRO_PRESERVE_TOOL_HISTORY", "on")

	msgs := []ClaudeMessage{
		{Role: "user", Content: "use a tool"},
		{Role: "assistant", Content: []interface{}{
			map[string]interface{}{"type": "text", "text": "calling exec"},
			map[string]interface{}{"type": "tool_use", "id": "t0", "name": "exec_command", "input": map[string]interface{}{"cmd": "ls"}},
		}},
		{Role: "user", Content: []interface{}{
			map[string]interface{}{"type": "tool_result", "tool_use_id": "t0", "content": "file1\nfile2"},
		}},
		// Final user turn with no tool_result — no tools requested this time.
		{Role: "user", Content: "now summarize without tools"},
	}

	// No Tools field — simulates a request that doesn't want tool use.
	payload := ClaudeToKiro(&ClaudeRequest{Model: "claude-opus-4.8", Messages: msgs}, false)

	// History must still have the structured toolUse (not flattened).
	var structuredCalls int
	for _, h := range payload.ConversationState.History {
		if a := h.AssistantResponseMessage; a != nil {
			structuredCalls += len(a.ToolUses)
		}
	}
	if structuredCalls != 1 {
		t.Fatalf("expected structured toolUse preserved in history, got %d", structuredCalls)
	}

	// UserInputMessageContext must have stub tools to satisfy Bedrock.
	ctx := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if ctx == nil || len(ctx.Tools) == 0 {
		t.Fatalf("expected stub toolConfig injected when no tools in request but history has structured blocks")
	}
	if ctx.Tools[0].ToolSpecification.Name != "exec_command" {
		t.Fatalf("expected stub tool named 'exec_command', got %q", ctx.Tools[0].ToolSpecification.Name)
	}
}
