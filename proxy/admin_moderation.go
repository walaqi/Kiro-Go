package proxy

import (
	"encoding/json"
	"net/http"
	"strings"

	"kiro-go/config"
)

// moderationView is the GET payload for the moderation gateway config. ForwardKey
// is masked so admins can confirm a key is set without exposing the secret,
// mirroring the API-key masking convention. ForwardKeySet lets the UI show
// whether a key exists at all (mask of an empty string is empty).
type moderationView struct {
	Enabled        bool               `json:"enabled"`
	JudgeModel     string             `json:"judgeModel"`
	Rules          []config.JudgeRule `json:"rules"`
	ForwardURL     string             `json:"forwardUrl"`
	ForwardKeyMask string             `json:"forwardKeyMasked"`
	ForwardKeySet  bool               `json:"forwardKeySet"`
}

func toModerationView(mc config.ModerationConfig) moderationView {
	rules := mc.Rules
	if rules == nil {
		rules = []config.JudgeRule{}
	}
	return moderationView{
		Enabled:        mc.Enabled,
		JudgeModel:     mc.JudgeModel,
		Rules:          rules,
		ForwardURL:     mc.ForwardURL,
		ForwardKeyMask: config.MaskApiKey(mc.ForwardKey),
		ForwardKeySet:  strings.TrimSpace(mc.ForwardKey) != "",
	}
}

func (h *Handler) apiGetModeration(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(toModerationView(config.GetModerationConfig()))
}

// moderationUpdateRequest mirrors ModerationConfig but takes ForwardKey as a
// pointer so we can distinguish "not sent / unchanged" from "explicitly cleared".
// When ForwardKey is nil or equals the current masked value, the stored secret is
// preserved — the UI never sees the cleartext key, so it round-trips the mask.
type moderationUpdateRequest struct {
	Enabled    bool               `json:"enabled"`
	JudgeModel string             `json:"judgeModel"`
	Rules      []config.JudgeRule `json:"rules"`
	ForwardURL string             `json:"forwardUrl"`
	ForwardKey *string            `json:"forwardKey,omitempty"`
}

func (h *Handler) apiUpdateModeration(w http.ResponseWriter, r *http.Request) {
	var req moderationUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	current := config.GetModerationConfig()

	// Resolve ForwardKey: preserve the stored secret unless the client sent a new
	// cleartext value. A nil field, an empty string, or the masked round-trip all
	// mean "keep existing".
	forwardKey := current.ForwardKey
	if req.ForwardKey != nil {
		v := strings.TrimSpace(*req.ForwardKey)
		if v != "" && v != config.MaskApiKey(current.ForwardKey) {
			forwardKey = v
		}
	}

	mc := config.ModerationConfig{
		Enabled:    req.Enabled,
		JudgeModel: req.JudgeModel,
		Rules:      req.Rules,
		ForwardURL: req.ForwardURL,
		ForwardKey: forwardKey,
	}

	if err := config.UpdateModerationConfig(mc); err != nil {
		// Save-time validation failures (missing required fields on an enabled
		// config, invalid ForwardURL) surface as 400 so the UI can show them.
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"moderation": toModerationView(config.GetModerationConfig()),
	})
}
