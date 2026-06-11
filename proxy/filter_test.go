package proxy

import (
	"kiro-go/config"
	"path/filepath"
	"testing"
)

func initTestConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := config.Init(path); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
}

func TestApplyClaudeFilters_Prepend(t *testing.T) {
	fc := config.FilterConfig{
		SystemInjection: config.SystemPromptInjection{
			Enabled:  true,
			Position: "prepend",
			Text:     "[INJECTED]",
		},
	}
	got := applyClaudeFilters("Hello world", fc)
	want := "[INJECTED]\n\nHello world"
	if got != want {
		t.Errorf("prepend:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestApplyClaudeFilters_Append(t *testing.T) {
	fc := config.FilterConfig{
		SystemInjection: config.SystemPromptInjection{
			Enabled:  true,
			Position: "append",
			Text:     "[FOOTER]",
		},
	}
	got := applyClaudeFilters("Hello world", fc)
	want := "Hello world\n\n[FOOTER]"
	if got != want {
		t.Errorf("append:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestApplyClaudeFilters_Disabled(t *testing.T) {
	fc := config.FilterConfig{
		SystemInjection: config.SystemPromptInjection{
			Enabled:  false,
			Position: "prepend",
			Text:     "[SHOULD NOT APPEAR]",
		},
	}
	got := applyClaudeFilters("Hello", fc)
	if got != "Hello" {
		t.Errorf("disabled injection appeared: %q", got)
	}
}

func TestApplyClaudeFilters_StringReplace(t *testing.T) {
	fc := config.FilterConfig{
		SystemReplaceRules: []config.SystemPromptReplaceRule{
			{ID: "1", Match: "secret", Replace: "[REDACTED]", Enabled: true},
			{ID: "2", Match: "foo", Replace: "bar", Enabled: true},
		},
	}
	got := applyClaudeFilters("This is secret and foo data", fc)
	want := "This is [REDACTED] and bar data"
	if got != want {
		t.Errorf("replace:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestApplyClaudeFilters_ReplaceOrdering(t *testing.T) {
	fc := config.FilterConfig{
		SystemReplaceRules: []config.SystemPromptReplaceRule{
			{ID: "a", Match: "ab", Replace: "x", Enabled: true},
			{ID: "b", Match: "xc", Replace: "Z", Enabled: true},
		},
	}
	got := applyClaudeFilters("abc", fc)
	if got != "Z" {
		t.Errorf("ordering: got %q, want %q", got, "Z")
	}
}

func TestApplyClaudeFilters_DisabledRuleSkipped(t *testing.T) {
	fc := config.FilterConfig{
		SystemReplaceRules: []config.SystemPromptReplaceRule{
			{ID: "1", Match: "hello", Replace: "bye", Enabled: false},
		},
	}
	got := applyClaudeFilters("hello world", fc)
	if got != "hello world" {
		t.Errorf("disabled rule applied: %q", got)
	}
}

func TestApplyClaudeFilters_EmptyMatchSkipped(t *testing.T) {
	fc := config.FilterConfig{
		SystemReplaceRules: []config.SystemPromptReplaceRule{
			{ID: "1", Match: "", Replace: "injected", Enabled: true},
		},
	}
	got := applyClaudeFilters("unchanged", fc)
	if got != "unchanged" {
		t.Errorf("empty match modified prompt: %q", got)
	}
}

func TestToolDescOverrideMap(t *testing.T) {
	rules := []config.ToolDescReplaceRule{
		{ID: "1", ToolName: "Read", Description: "Reads a file", Enabled: true},
		{ID: "2", ToolName: "Write", Description: "Writes a file", Enabled: false},
		{ID: "3", ToolName: "", Description: "empty name", Enabled: true},
	}
	m := toolDescOverrideMap(rules)
	if m["Read"] != "Reads a file" {
		t.Errorf("Read override missing or wrong: %v", m)
	}
	if _, ok := m["Write"]; ok {
		t.Error("disabled Write rule should not appear")
	}
	if len(m) != 1 {
		t.Errorf("expected 1 entry, got %d: %v", len(m), m)
	}
}

func TestConvertClaudeTools_WithOverride(t *testing.T) {
	rules := []config.ToolDescReplaceRule{
		{ID: "1", ToolName: "Bash", Description: "Run shell commands", Enabled: true},
	}
	tools := []ClaudeTool{
		{Name: "Bash", Description: "Original description", InputSchema: map[string]interface{}{"type": "object"}},
		{Name: "Read", Description: "Read a file", InputSchema: map[string]interface{}{"type": "object"}},
	}
	result, _ := convertClaudeTools(tools, rules)
	if len(result) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(result))
	}
	if result[0].ToolSpecification.Description != "Run shell commands" {
		t.Errorf("Bash desc not overridden: %q", result[0].ToolSpecification.Description)
	}
	if result[1].ToolSpecification.Description != "Read a file" {
		t.Errorf("Read desc changed unexpectedly: %q", result[1].ToolSpecification.Description)
	}
}
