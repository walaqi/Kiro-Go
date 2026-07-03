package config

import (
	"testing"
)

func TestModerationConfigValidation(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := Init(cfgFile); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Disabled config: blank fields are fine.
	if err := UpdateModerationConfig(ModerationConfig{Enabled: false}); err != nil {
		t.Fatalf("disabled config should save: %v", err)
	}

	// Enabled but missing JudgeModel → rejected.
	if err := UpdateModerationConfig(ModerationConfig{
		Enabled:    true,
		ForwardURL: "https://x/v1/messages",
		ForwardKey: "k",
	}); err == nil {
		t.Fatal("expected error for missing judgeModel")
	}

	// Enabled but missing ForwardKey → rejected.
	if err := UpdateModerationConfig(ModerationConfig{
		Enabled:    true,
		JudgeModel: "claude-haiku-4.5",
		ForwardURL: "https://x/v1/messages",
	}); err == nil {
		t.Fatal("expected error for missing forwardKey")
	}

	// Enabled but invalid ForwardURL → rejected.
	if err := UpdateModerationConfig(ModerationConfig{
		Enabled:    true,
		JudgeModel: "claude-haiku-4.5",
		ForwardURL: "not-a-url",
		ForwardKey: "k",
	}); err == nil {
		t.Fatal("expected error for invalid forwardUrl")
	}

	// Fully valid enabled config → saved and ready.
	if err := UpdateModerationConfig(ModerationConfig{
		Enabled:    true,
		JudgeModel: "claude-haiku-4.5",
		ForwardURL: "https://x/v1/messages",
		ForwardKey: "k",
	}); err != nil {
		t.Fatalf("valid config should save: %v", err)
	}
	if !GetModerationConfig().ModerationReady() {
		t.Fatal("expected ready config after valid save")
	}
}

func TestModerationReadyRejectsIncomplete(t *testing.T) {
	cases := []ModerationConfig{
		{Enabled: false, JudgeModel: "m", ForwardURL: "https://x", ForwardKey: "k"}, // disabled
		{Enabled: true, ForwardURL: "https://x", ForwardKey: "k"},                   // no model
		{Enabled: true, JudgeModel: "m", ForwardKey: "k"},                           // no url
		{Enabled: true, JudgeModel: "m", ForwardURL: "https://x"},                   // no key
	}
	for i, c := range cases {
		if c.ModerationReady() {
			t.Fatalf("case %d should not be ready: %+v", i, c)
		}
	}
	ready := ModerationConfig{Enabled: true, JudgeModel: "m", ForwardURL: "https://x", ForwardKey: "k"}
	if !ready.ModerationReady() {
		t.Fatal("complete enabled config should be ready")
	}
}

// TestUpdateApiKeyPersistsModeration is the regression guard for the field-drop
// bug: UpdateApiKey overwrites fields explicitly, so a newly added field must be
// wired in or it silently vanishes on update.
func TestUpdateApiKeyPersistsModeration(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := Init(cfgFile); err != nil {
		t.Fatalf("Init: %v", err)
	}
	entry, err := AddApiKey(ApiKeyEntry{Key: "key-abcdefghij", Enabled: true, Moderation: false})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}

	patch := *GetApiKeyEntry(entry.ID)
	patch.Moderation = true
	if err := UpdateApiKey(entry.ID, patch); err != nil {
		t.Fatalf("UpdateApiKey: %v", err)
	}

	got := GetApiKeyEntry(entry.ID)
	if got == nil || !got.Moderation {
		t.Fatalf("Moderation flag lost on update: %+v", got)
	}

	// Flip back to false and confirm it also persists (not just a one-way latch).
	patch.Moderation = false
	if err := UpdateApiKey(entry.ID, patch); err != nil {
		t.Fatalf("UpdateApiKey (unset): %v", err)
	}
	if GetApiKeyEntry(entry.ID).Moderation {
		t.Fatal("Moderation should be false after unset")
	}
}

func TestAddApiKeyPersistsModeration(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := Init(cfgFile); err != nil {
		t.Fatalf("Init: %v", err)
	}
	entry, err := AddApiKey(ApiKeyEntry{Key: "key-zzzzzzzzzz", Enabled: true, Moderation: true})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}
	if !GetApiKeyEntry(entry.ID).Moderation {
		t.Fatal("Moderation flag not persisted on create")
	}
}
