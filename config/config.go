// Package config provides configuration management for Kiro API Proxy.
//
// This package handles persistent storage and retrieval of:
//   - Account credentials and authentication tokens
//   - Server settings (port, host, API keys)
//   - Usage statistics and metrics
//   - Thinking mode configuration for AI responses
//
// All configuration is stored in a JSON file with thread-safe access
// via read-write mutex protection.
package config

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// GenerateMachineId generates a UUID v4 format machine identifier.
// This ID is used to uniquely identify the proxy instance in Kiro API requests,
// helping with request tracking and rate limiting on the server side.
func GenerateMachineId() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	bytes[6] = (bytes[6] & 0x0f) | 0x40 // 版本 4
	bytes[8] = (bytes[8] & 0x3f) | 0x80 // 变体
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

// Account represents a Kiro API account with authentication credentials and usage statistics.
type Account struct {
	// Basic identification
	ID       string `json:"id"`                 // Unique account identifier (UUID)
	Email    string `json:"email,omitempty"`    // User email address
	UserId   string `json:"userId,omitempty"`   // Kiro user ID
	Nickname string `json:"nickname,omitempty"` // Display name for admin panel

	// Authentication credentials
	AccessToken  string `json:"accessToken"`            // OAuth access token for API calls
	RefreshToken string `json:"refreshToken"`           // OAuth refresh token for token renewal
	ClientID     string `json:"clientId,omitempty"`     // OIDC client ID (for IdC auth)
	ClientSecret string `json:"clientSecret,omitempty"` // OIDC client secret (for IdC auth)
	AuthMethod   string `json:"authMethod"`             // Authentication method: "idc" (AWS IdC) or "social" (GitHub/Google)
	Provider     string `json:"provider,omitempty"`     // Identity provider name (e.g., "BuilderId", "GitHub")
	Region       string `json:"region"`                 // AWS region for OIDC endpoints
	StartUrl     string `json:"startUrl,omitempty"`     // AWS SSO start URL
	ExpiresAt    int64  `json:"expiresAt,omitempty"`    // Token expiration timestamp (Unix seconds)
	MachineId    string `json:"machineId,omitempty"`    // UUID machine identifier for request tracking
	ProfileArn   string `json:"profileArn,omitempty"`   // CodeWhisperer/Kiro profile ARN for generation requests

	// Per-account outbound proxy (falls back to global ProxyURL if empty)
	ProxyURL string `json:"proxyURL,omitempty"`

	// Priority weight for load balancing (higher = more requests)
	Weight int `json:"weight,omitempty"` // 0 or 1 = normal, 2+ = higher priority

	// Upstream Overages state (mirrored from AWS Q `setUserPreference` / `getUsageLimits`).
	// OverageStatus is the only switch that decides whether to keep dispatching once UsageLimit is reached.
	// Allowed values: "ENABLED", "DISABLED", "UNKNOWN" (or empty when not yet fetched).
	OverageStatus     string  `json:"overageStatus,omitempty"`
	OverageCapability string  `json:"overageCapability,omitempty"` // "OVERAGE_CAPABLE" / "NOT_OVERAGE_CAPABLE"
	OverageCap        float64 `json:"overageCap,omitempty"`        // Hard upper bound (USD)
	OverageRate       float64 `json:"overageRate,omitempty"`       // Per-invocation rate (USD)
	CurrentOverages   float64 `json:"currentOverages,omitempty"`   // Cumulative overage charges (USD)
	OverageCheckedAt  int64   `json:"overageCheckedAt,omitempty"`  // Last successful upstream sync (Unix seconds)

	// LegacyAllowOverage is kept for backward-compatible JSON loading only.
	// Pre-Overages-switch deployments persisted `allowOverage: true` to mean
	// "keep dispatching when quota is exhausted". On first load we migrate it
	// into OverageStatus="ENABLED" and zero this field so it does not get
	// re-emitted on future saves. Do not read this field elsewhere.
	LegacyAllowOverage bool `json:"allowOverage,omitempty"`

	// Account status
	Enabled   bool   `json:"enabled"`             // Whether account is active in the pool
	BanStatus string `json:"banStatus,omitempty"` // Ban status: "ACTIVE", "BANNED", "SUSPENDED"
	BanReason string `json:"banReason,omitempty"` // Reason for ban/suspension
	BanTime   int64  `json:"banTime,omitempty"`   // Timestamp when ban was detected

	// Subscription information
	SubscriptionType  string `json:"subscriptionType,omitempty"`  // Tier: FREE, PRO, PRO_PLUS, or POWER
	SubscriptionTitle string `json:"subscriptionTitle,omitempty"` // Human-readable subscription name
	DaysRemaining     int    `json:"daysRemaining,omitempty"`     // Days until subscription expires

	// Usage tracking
	UsageCurrent  float64 `json:"usageCurrent,omitempty"`  // Current period usage (credits)
	UsageLimit    float64 `json:"usageLimit,omitempty"`    // Maximum allowed usage per period
	UsagePercent  float64 `json:"usagePercent,omitempty"`  // Usage percentage (0.0-1.0)
	NextResetDate string  `json:"nextResetDate,omitempty"` // Date when usage resets (YYYY-MM-DD)
	LastRefresh   int64   `json:"lastRefresh,omitempty"`   // Last info refresh timestamp

	// Trial usage tracking
	TrialUsageCurrent float64 `json:"trialUsageCurrent,omitempty"` // Trial quota current usage
	TrialUsageLimit   float64 `json:"trialUsageLimit,omitempty"`   // Trial quota total limit
	TrialUsagePercent float64 `json:"trialUsagePercent,omitempty"` // Trial quota usage percentage (0.0-1.0)
	TrialStatus       string  `json:"trialStatus,omitempty"`       // Trial status: ACTIVE, EXPIRED, NONE
	TrialExpiresAt    int64   `json:"trialExpiresAt,omitempty"`    // Trial expiration timestamp (Unix seconds)

	// Runtime statistics (updated during operation). Persisted to the sibling
	// state file (see state.go), not config.json — hence json:"-". These remain
	// the authoritative in-memory copy read by the admin panel and pool seeding.
	RequestCount int     `json:"-"` // Total requests processed
	ErrorCount   int     `json:"-"` // Total errors encountered
	LastUsed     int64   `json:"-"` // Last request timestamp
	TotalTokens  int     `json:"-"` // Cumulative tokens processed
	TotalCredits float64 `json:"-"` // Cumulative credits consumed
}

// SystemPromptInjection appends or prepends a fixed text block to the Claude
// system prompt. Applied as step 1 of the filter pipeline.
type SystemPromptInjection struct {
	Enabled  bool   `json:"enabled"`
	Position string `json:"position"` // "prepend" or "append"
	Text     string `json:"text"`
}

// SystemPromptReplaceRule is a plain-string find/replace applied to the Claude
// system prompt. Rules are applied in list order. An empty Replace deletes the
// match. Applied as step 2 of the filter pipeline.
type SystemPromptReplaceRule struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Match   string `json:"match"`
	Replace string `json:"replace,omitempty"`
	Enabled bool   `json:"enabled"`
}

// ToolDescReplaceRule wholly replaces a Claude tool's description, matched by
// the tool's original name (exact match). Applied as step 3 of the filter
// pipeline.
type ToolDescReplaceRule struct {
	ID          string `json:"id"`
	ToolName    string `json:"toolName"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
}

// FilterConfig groups all Claude-path filter settings exposed via the admin
// "过滤" tab.
type FilterConfig struct {
	SystemInjection      SystemPromptInjection     `json:"systemInjection"`
	SystemReplaceRules   []SystemPromptReplaceRule `json:"systemReplaceRules"`
	ToolDescReplaceRules []ToolDescReplaceRule     `json:"toolDescReplaceRules"`
}

// JudgeRule is a single intent-moderation rubric. The LLM judge is shown all
// enabled rules (numbered) and asked to return the number(s) of any it matches,
// or 0 for none. Criteria is the human-authored description of what to flag.
type JudgeRule struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Enabled  bool   `json:"enabled"`
	Criteria string `json:"criteria"` // 判断标准/rubric
}

// ModerationConfig configures the intent-moderation + hit-forward gateway
// (Claude /v1/messages path only). When enabled AND a request's API key has
// Moderation=true AND the request carries the X-Origin-Model-Id header, the
// latest user message is classified by a cheap judge model; on a hit the whole
// request is forwarded to ForwardURL instead of going through the normal Kiro
// flow. See docs/plans/intent-moderation-forward-gateway.md.
type ModerationConfig struct {
	Enabled    bool        `json:"enabled"`    // 全局总闸
	JudgeModel string      `json:"judgeModel"` // 如 claude-haiku-4.5
	Rules      []JudgeRule `json:"rules"`
	ForwardURL string      `json:"forwardUrl"` // 命中转发的固定完整 URL
	ForwardKey string      `json:"forwardKey"` // 转发专用鉴权 key(密钥,UI 脱敏)

	// ForwardFullContent controls what a moderation hit forwards. Default (false):
	// data-minimized — only the latest user message is sent (see rewriteForwardBody).
	// When true: forward the full original request body (history, system, tools all
	// preserved), swapping only the model. Hot-reloaded per request like the other
	// fields; no restart needed.
	ForwardFullContent bool `json:"forwardFullContent"`
}

// ModerationReady reports whether the moderation gateway is fully configured to
// run. A residual/incomplete config (Enabled=true but a required field empty) is
// treated as "not ready" and the request falls through to the normal flow
// (fail-open) — an unconfigured gateway is not a running gateway, and must not
// blanket-block whitelisted keys. This is orthogonal to the fail-closed applied
// when a judge call actually fails at runtime.
func (m ModerationConfig) ModerationReady() bool {
	return m.Enabled &&
		strings.TrimSpace(m.JudgeModel) != "" &&
		strings.TrimSpace(m.ForwardURL) != "" &&
		strings.TrimSpace(m.ForwardKey) != ""
}

// ApiKeyEntry represents a single API key with optional usage limits and counters.
// Limits with value 0 are treated as "no limit". Counters are cumulative and never reset
// automatically; operators can use the admin endpoint to manually reset them.
type ApiKeyEntry struct {
	ID         string `json:"id"`                 // Unique identifier (UUID)
	Name       string `json:"name,omitempty"`     // Human-readable label
	Key        string `json:"key"`                // The actual key value clients send
	Enabled    bool   `json:"enabled"`            // Whether this key may authenticate
	Migrated   bool   `json:"migrated,omitempty"` // True if migrated from legacy single ApiKey field
	CreatedAt  int64  `json:"createdAt"`          // Creation timestamp (Unix seconds)
	LastUsedAt int64  `json:"-"`                  // Persisted to state file, not config.json

	// Moderation opts this key into the intent-moderation gateway (whitelist-style,
	// default false). Only requests from a key with Moderation=true are ever
	// classified/forwarded, and only when the global ModerationConfig is enabled
	// and ready. See docs/plans/intent-moderation-forward-gateway.md.
	Moderation bool `json:"moderation,omitempty"`

	// Limits (0 = unlimited)
	TokenLimit  int64   `json:"tokenLimit,omitempty"`
	CreditLimit float64 `json:"creditLimit,omitempty"`

	// Cumulative usage (never auto-reset). Persisted to the sibling state file
	// (see state.go), not config.json — hence json:"-".
	TokensUsed    int64   `json:"-"`
	CreditsUsed   float64 `json:"-"`
	RequestsCount int64   `json:"-"`
}

// PricingConfig holds the upstream source URLs for the model pricing table used
// by credit-to-USD usage calibration. When a URL is empty the background updater
// falls back to its built-in default (see DefaultPricingHashURL / DefaultPricingURL).
type PricingConfig struct {
	HashURL    string `json:"hash_url,omitempty"`    // URL of the upstream sha256 of the pricing document
	PricingURL string `json:"pricing_url,omitempty"` // URL of the upstream pricing JSON document
}

// DefaultPricingHashURL and DefaultPricingURL are the built-in upstream sources
// for the model pricing table. They are written into a freshly created config on
// cold start and serve as the fallback when the pricing URLs are left empty.
// The gh-proxy.org prefix fronts raw.githubusercontent.com for reachability in
// regions where the bare GitHub raw host is blocked.
const (
	DefaultPricingHashURL = "https://raw.githubusercontent.com/walaqi/model-price-repo/refs/heads/main/model_prices_and_context_window.sha256"
	DefaultPricingURL     = "https://raw.githubusercontent.com/walaqi/model-price-repo/refs/heads/main/model_prices_and_context_window.json"
)

// defaultPricingConfig returns a PricingConfig populated with the built-in
// upstream sources.
func defaultPricingConfig() *PricingConfig {
	return &PricingConfig{
		HashURL:    DefaultPricingHashURL,
		PricingURL: DefaultPricingURL,
	}
}

// Config represents the global application configuration.
type Config struct {
	// Server settings
	Password      string        `json:"password"`          // Admin panel password
	Port          int           `json:"port"`              // HTTP server port (default: 8080)
	Host          string        `json:"host"`              // HTTP server bind address (default: 0.0.0.0)
	ApiKey        string        `json:"apiKey,omitempty"`  // [Deprecated] Legacy single API key, migrated into ApiKeys on first load
	RequireApiKey bool          `json:"requireApiKey"`     // [Deprecated] Whether to enforce API key validation; with multi-key support, len(ApiKeys)>0 implicitly enforces auth
	ApiKeys       []ApiKeyEntry `json:"apiKeys,omitempty"` // Multiple API keys, each with independent quota
	KiroVersion   string        `json:"kiroVersion,omitempty"`
	SystemVersion string        `json:"systemVersion,omitempty"`
	NodeVersion   string        `json:"nodeVersion,omitempty"`
	Accounts      []Account     `json:"accounts"` // Registered Kiro accounts

	// Thinking mode configuration for extended reasoning output
	ThinkingSuffix       string `json:"thinkingSuffix,omitempty"`       // Model suffix to trigger thinking mode (default: "-thinking")
	OpenAIThinkingFormat string `json:"openaiThinkingFormat,omitempty"` // OpenAI output format: "reasoning_content", "thinking", or "think"
	ClaudeThinkingFormat string `json:"claudeThinkingFormat,omitempty"` // Claude output format: "reasoning_content", "thinking", or "think"

	// Endpoint configuration: "auto", "kiro", "codewhisperer", or "amazonq"
	PreferredEndpoint string `json:"preferredEndpoint,omitempty"`

	// EndpointFallback controls whether to try other endpoints when the preferred one fails.
	// Defaults to true. Set to false to only use the preferred endpoint.
	EndpointFallback *bool `json:"endpointFallback,omitempty"`

	// AllowOverUsage allows accounts to continue serving requests even when their
	// usage quota has been exhausted. When enabled, the pool will not skip accounts
	// solely because usageCurrent >= usageLimit.
	AllowOverUsage bool `json:"allowOverUsage,omitempty"`

	// ToolLeakFix controls the cross-frame filter that rescues tool-call XML the
	// Kiro backend occasionally leaks into assistant text (see proxy/tool_leak_filter.go).
	// Defaults to true. Set to false to fall back to passing assistant text through
	// unfiltered. The KIRO_TOOL_LEAK_FIX env var, when set, overrides this.
	ToolLeakFix *bool `json:"toolLeakFix,omitempty"`

	// ToolLeakDebug enables verbose logging for the tool-call XML leak filter
	// (per-leak parse logs and forced rescue/dedup stats). Defaults to false.
	// The KIRO_TOOL_LEAK_DEBUG=1 env var, when set, forces this on.
	ToolLeakDebug bool `json:"toolLeakDebug,omitempty"`

	// PreserveToolHistory controls how structured tool calls/results in the
	// conversation history are sent upstream (see proxy/translator.go
	// sanitizeKiroHistory). When true (the default), complete structured tool
	// pairs are kept intact, matching what the real Kiro IDE client sends — this
	// is the root-cause fix for the transcript-leak imitation that flattening
	// causes. When false, the proxy falls back to the conservative original
	// behavior: keep at most one active structured pair and narrate the rest to
	// text. Defaults to true. Set to false (or KIRO_PRESERVE_TOOL_HISTORY=off)
	// to fall back instantly if the upstream rejects the preserved-structure
	// payload, without a new release.
	PreserveToolHistory *bool `json:"preserveToolHistory,omitempty"`

	// CreditsToUSD is the conversion constant from upstream Kiro credits to USD,
	// used to calibrate the reported output / cache_read / cache_creation token
	// counts against published model list prices. The target dollar amount for a
	// request is credits * CreditsToUSD. A value <= 0 falls back to the default
	// (see GetCreditsToUSD). This only affects the usage numbers reported to
	// clients; it does not change credit accounting or persisted statistics.
	CreditsToUSD float64 `json:"creditsToUSD,omitempty"`

	// CacheReadBias shifts a fraction of inferred cache_creation tokens into
	// cache_read before the credit calibration scale is solved. Range [0, 1):
	// 0 leaves the cache_tracker breakdown untouched (default), values closer
	// to 1 move more of the creation total into read so the displayed cache
	// hit ratio is higher. The total list-price cost is still rebalanced to
	// match credits*CreditsToUSD via the calibration scale, so input/output
	// and overall billed dollars are unchanged. Like CreditsToUSD this is a
	// server-side display knob and is intentionally not exposed in the admin
	// UI; tune it via config.json after sampling real requests.
	CacheReadBias float64 `json:"cacheReadBias,omitempty"`

	// Pricing configures the source URLs for the background pricing-table updater
	// (see proxy/pricing_updater.go). When unset, the updater falls back to its
	// built-in default URLs. Useful for pointing at a mirror/proxy of the
	// upstream pricing document.
	Pricing *PricingConfig `json:"pricing,omitempty"`

	// Proxy configuration: optional outbound proxy for Kiro API requests
	// Format: "socks5://host:port", "socks5://user:pass@host:port",
	//         "http://host:port",  "http://user:pass@host:port"
	// Leave empty to connect directly.
	ProxyURL string `json:"proxyURL,omitempty"`

	// Filter holds Claude-path system-prompt and tool-description rewrite rules,
	// configured from the admin "过滤" tab.
	Filter *FilterConfig `json:"filter,omitempty"`

	// Moderation holds the intent-moderation + hit-forward gateway config
	// (Claude /v1/messages path only), configured from the admin "审核" tab.
	Moderation *ModerationConfig `json:"moderation,omitempty"`

	// Timezone is used for date-based aggregation (daily stats files, etc.).
	// Accepts any IANA timezone name (e.g. "Asia/Shanghai", "America/New_York").
	// Defaults to "Asia/Shanghai" when empty.
	Timezone string `json:"timezone,omitempty"`

	// LogLevel controls verbosity of application logs.
	// Accepted values: "debug", "info", "warn", "error". Defaults to "info".
	// Can be overridden by the LOG_LEVEL environment variable.
	LogLevel string `json:"logLevel,omitempty"`

	// Global statistics. Persisted to the sibling state file (see state.go), not
	// config.json — hence json:"-". They remain the authoritative in-memory copy
	// seeded into the handler on startup via GetStats.
	TotalRequests   int     `json:"-"` // Total API requests received
	SuccessRequests int     `json:"-"` // Successful requests count
	FailedRequests  int     `json:"-"` // Failed requests count
	TotalTokens     int     `json:"-"` // Total tokens processed
	TotalCredits    float64 `json:"-"` // Total credits consumed
}

// AccountInfo contains account metadata retrieved from Kiro API.
// Used for updating subscription and usage information.
type AccountInfo struct {
	Email             string
	UserId            string
	SubscriptionType  string
	SubscriptionTitle string
	DaysRemaining     int
	UsageCurrent      float64
	UsageLimit        float64
	UsagePercent      float64
	NextResetDate     string
	LastRefresh       int64
	TrialUsageCurrent float64
	TrialUsageLimit   float64
	TrialUsagePercent float64
	TrialStatus       string
	TrialExpiresAt    int64
}

// Version current version
const Version = "1.1.2"

// defaultCreditsToUSD is the fallback conversion constant from upstream Kiro
// credits to USD, used when CreditsToUSD is unset or non-positive.
const defaultCreditsToUSD = 0.2

var (
	cfg     *Config
	cfgLock sync.RWMutex
	cfgPath string
)

// Init initializes the configuration system with the specified file path.
// If the file doesn't exist, a default configuration is created.
func Init(path string) error {
	cfgPath = path
	statePath = deriveStatePath(path)
	return Load()
}

func Load() error {
	cfgLock.Lock()
	defer cfgLock.Unlock()

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Create default configuration.
			// Binds to 0.0.0.0 by default for Docker/container compatibility.
			cfg = &Config{
				Password:      "changeme",
				Port:          8080,
				Host:          "0.0.0.0",
				RequireApiKey: false,
				Accounts:      []Account{},
				Pricing:       defaultPricingConfig(),
			}
			if err := saveLocked(); err != nil {
				return err
			}
			// Fresh install: no legacy counters to migrate. Initialize an empty
			// state file alongside the new config. loadStateLocked applies the
			// (empty) state into cfg internally.
			if _, err := loadStateLocked(nil); err != nil {
				return err
			}
			return nil
		}
		return err
	}

	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	cfg = &c

	// Migration: if a legacy single ApiKey is present and the new ApiKeys list is empty,
	// promote it into the new structure. The migrated entry inherits the legacy
	// RequireApiKey state — if the legacy deployment was public (RequireApiKey=false),
	// we mark the entry disabled so it doesn't accidentally start enforcing auth.
	// Operators can flip it on later from the admin UI. The legacy field is kept
	// for backward compatibility when reading older config files.
	if cfg.ApiKey != "" && len(cfg.ApiKeys) == 0 {
		cfg.ApiKeys = append(cfg.ApiKeys, ApiKeyEntry{
			ID:        newUUID(),
			Name:      "legacy",
			Key:       cfg.ApiKey,
			Enabled:   cfg.RequireApiKey,
			Migrated:  true,
			CreatedAt: time.Now().Unix(),
		})
		if err := saveLocked(); err != nil {
			return err
		}
	}

	// Migration: per-account AllowOverage → OverageStatus.
	// Pre-Overages-switch deployments stored `allowOverage: true` to mean "keep
	// dispatching when quota is exhausted". The new model reads OverageStatus
	// from the upstream AWS Q switch instead. To avoid silently disabling
	// previously-allowed accounts on first launch, treat allowOverage=true as
	// OverageStatus="ENABLED" (operators can refresh from AWS later). The
	// legacy field is then cleared so future saves don't re-emit it.
	overageMigrated := false
	for i := range cfg.Accounts {
		if cfg.Accounts[i].LegacyAllowOverage {
			if cfg.Accounts[i].OverageStatus == "" {
				cfg.Accounts[i].OverageStatus = "ENABLED"
			}
			cfg.Accounts[i].LegacyAllowOverage = false
			overageMigrated = true
		}
	}
	if overageMigrated {
		if err := saveLocked(); err != nil {
			return err
		}
	}

	// Initialize the runtime-state store. When state.json is absent, seed it from
	// any inline counters still present in the raw config bytes (older configs).
	// Because the stat fields are now json:"-", they were not loaded into cfg by
	// the unmarshal above; loadStateLocked restores them into cfg from state.
	stateMigrated, err := loadStateLocked(data)
	if err != nil {
		return err
	}

	// When we just migrated inline counters out of a legacy config.json, rewrite
	// it so the now-orphaned stat fields are dropped from disk on first launch
	// (they live in state.json from here on).
	if stateMigrated {
		if err := saveLocked(); err != nil {
			return err
		}
	}
	return nil
}

// saveLocked persists cfg to disk. Caller MUST already hold cfgLock.
// This is identical to Save() (which does not take the lock either) but is named
// distinctly so call sites that already hold cfgLock are explicit about it.
func saveLocked() error {
	return Save()
}

// newUUID returns a UUID v4 string. Defined here to avoid pulling extra deps in this file.
func newUUID() string {
	return GenerateMachineId()
}

// Save persists the current configuration to the JSON file.
// Uses indented formatting for human readability. The write is atomic: data is
// written to a temp file and renamed over the target, so a crash mid-write can
// never leave a truncated or corrupt config.json (which holds refresh tokens).
func Save() error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(cfgPath, data, 0600)
}

// writeFileAtomic writes data to path atomically by writing to a sibling temp
// file in the same directory, fsync-ing it, then renaming over the target.
// rename(2) within a filesystem is atomic, so readers see either the old file
// or the fully-written new one, never a partial write. The temp file is removed
// on any error before the rename completes.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail out before the successful rename.
	defer func() {
		if _, statErr := os.Stat(tmpName); statErr == nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// SetPassword updates the admin password.
// Primarily used for environment variable override in containerized deployments.
func SetPassword(password string) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.Password = password
}

// GetConfigDir returns the directory containing the config JSON file.
// Useful for sibling state (e.g. stored Responses, caches) that should live
// alongside the configuration file.
func GetConfigDir() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfgPath == "" {
		return "."
	}
	dir := cfgPath
	for i := len(dir) - 1; i >= 0; i-- {
		if dir[i] == '/' || dir[i] == '\\' {
			return dir[:i]
		}
	}
	return "."
}

func Get() *Config {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg
}

func GetPassword() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.Password
}

func GetPort() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.Port == 0 {
		return 8080
	}
	return cfg.Port
}

func GetHost() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.Host == "" {
		return "127.0.0.1"
	}
	return cfg.Host
}

func GetAccounts() []Account {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	accounts := make([]Account, len(cfg.Accounts))
	copy(accounts, cfg.Accounts)
	return accounts
}

func GetEnabledAccounts() []Account {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	var accounts []Account
	for _, a := range cfg.Accounts {
		if a.Enabled {
			accounts = append(accounts, a)
		}
	}
	return accounts
}

func AddAccount(account Account) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.Accounts = append(cfg.Accounts, account)
	return Save()
}

func UpdateAccount(id string, account Account) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i] = account
			return Save()
		}
	}
	return nil
}

// UpdateAccountOverageStatus persists the cached upstream overage status fields.
// Called after a successful setUserPreference or getUsageLimits round-trip.
func UpdateAccountOverageStatus(id, status, capability string, cap, rate, current float64, checkedAt int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			if status != "" {
				cfg.Accounts[i].OverageStatus = status
			}
			if capability != "" {
				cfg.Accounts[i].OverageCapability = capability
			}
			cfg.Accounts[i].OverageCap = cap
			cfg.Accounts[i].OverageRate = rate
			cfg.Accounts[i].CurrentOverages = current
			if checkedAt > 0 {
				cfg.Accounts[i].OverageCheckedAt = checkedAt
			}
			return Save()
		}
	}
	return nil
}

// SetAccountEnabled toggles the enabled state of an account and persists the change.
// Used to disable accounts whose refresh token has been revoked (401 Bad credentials)
// so subsequent requests skip them automatically.
func SetAccountEnabled(id string, enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].Enabled = enabled
			if !enabled {
				cfg.Accounts[i].BanStatus = "DISABLED"
				cfg.Accounts[i].BanTime = time.Now().Unix()
			}
			return Save()
		}
	}
	return nil
}

// SetAccountBanStatus marks an account as banned/disabled with a reason.
// Reason is recorded so operators can see why the account was auto-disabled.
func SetAccountBanStatus(id, status, reason string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].BanStatus = status
			cfg.Accounts[i].BanReason = reason
			cfg.Accounts[i].BanTime = time.Now().Unix()
			if status == "BANNED" || status == "DISABLED" {
				cfg.Accounts[i].Enabled = false
			}
			return Save()
		}
	}
	return nil
}

func UpdateAccountProfileArn(id, profileArn string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].ProfileArn = profileArn
			return Save()
		}
	}
	return nil
}

func DeleteAccount(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts = append(cfg.Accounts[:i], cfg.Accounts[i+1:]...)
			if err := Save(); err != nil {
				return err
			}
			// state.json is derived from cfg at flush time, so re-flushing after
			// the account is gone automatically drops its persisted counters —
			// no orphan entries accumulate. Cold admin path, so the brief flush
			// under cfgLock is acceptable.
			return persistStateLocked()
		}
	}
	return nil
}

func UpdateAccountToken(id, accessToken, refreshToken string, expiresAt int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].AccessToken = accessToken
			if refreshToken != "" {
				cfg.Accounts[i].RefreshToken = refreshToken
			}
			cfg.Accounts[i].ExpiresAt = expiresAt
			return Save()
		}
	}
	return nil
}

func GetApiKey() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.ApiKey
}

func IsApiKeyRequired() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.RequireApiKey
}

func UpdateSettings(apiKey string, requireApiKey bool, password string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ApiKey = apiKey
	cfg.RequireApiKey = requireApiKey
	if password != "" {
		cfg.Password = password
	}
	return Save()
}

func UpdateSettingsPatch(apiKey *string, requireApiKey *bool, password string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if apiKey != nil {
		cfg.ApiKey = *apiKey
	}
	if requireApiKey != nil {
		cfg.RequireApiKey = *requireApiKey
	}
	if password != "" {
		cfg.Password = password
	}
	return Save()
}

// UpdateStats updates the process-wide totals. These are hot-path counters, so
// they are persisted to the sibling state file (state.go) rather than rewriting
// config.json. The in-memory cfg copy is kept in sync for GetStats readers.
func UpdateStats(totalReq, successReq, failedReq, totalTokens int, totalCredits float64) error {
	cfgLock.Lock()
	cfg.TotalRequests = totalReq
	cfg.SuccessRequests = successReq
	cfg.FailedRequests = failedReq
	cfg.TotalTokens = totalTokens
	cfg.TotalCredits = totalCredits
	cfgLock.Unlock()
	// Flush the derived snapshot to state.json. persistState re-snapshots cfg
	// under its own RLock, so cfgLock must already be released here.
	return persistState()
}

func GetStats() (int, int, int, int, float64) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.TotalRequests, cfg.SuccessRequests, cfg.FailedRequests, cfg.TotalTokens, cfg.TotalCredits
}

// UpdateAccountStats updates an account's runtime counters. Like UpdateStats,
// these are hot-path values persisted to the sibling state file rather than
// config.json. Returns nil (no-op) when the account ID is unknown.
func UpdateAccountStats(id string, requestCount, errorCount, totalTokens int, totalCredits float64, lastUsed int64) error {
	cfgLock.Lock()
	found := false
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].RequestCount = requestCount
			cfg.Accounts[i].ErrorCount = errorCount
			cfg.Accounts[i].TotalTokens = totalTokens
			cfg.Accounts[i].TotalCredits = totalCredits
			cfg.Accounts[i].LastUsed = lastUsed
			found = true
			break
		}
	}
	cfgLock.Unlock()
	if !found {
		return nil
	}
	return persistState()
}

// UpdateAccountInfo updates an account's subscription and usage information.
// Called after refreshing account data from Kiro API.
func UpdateAccountInfo(id string, info AccountInfo) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			if info.Email != "" {
				cfg.Accounts[i].Email = info.Email
			}
			if info.UserId != "" {
				cfg.Accounts[i].UserId = info.UserId
			}
			cfg.Accounts[i].SubscriptionType = info.SubscriptionType
			cfg.Accounts[i].SubscriptionTitle = info.SubscriptionTitle
			cfg.Accounts[i].DaysRemaining = info.DaysRemaining
			cfg.Accounts[i].UsageCurrent = info.UsageCurrent
			cfg.Accounts[i].UsageLimit = info.UsageLimit
			cfg.Accounts[i].UsagePercent = info.UsagePercent
			cfg.Accounts[i].NextResetDate = info.NextResetDate
			cfg.Accounts[i].LastRefresh = info.LastRefresh
			cfg.Accounts[i].TrialUsageCurrent = info.TrialUsageCurrent
			cfg.Accounts[i].TrialUsageLimit = info.TrialUsageLimit
			cfg.Accounts[i].TrialUsagePercent = info.TrialUsagePercent
			cfg.Accounts[i].TrialStatus = info.TrialStatus
			cfg.Accounts[i].TrialExpiresAt = info.TrialExpiresAt
			return Save()
		}
	}
	return nil
}

// GetFilterConfig returns a deep copy of the current filter configuration.
func GetFilterConfig() FilterConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.Filter == nil {
		return FilterConfig{
			SystemReplaceRules:   []SystemPromptReplaceRule{},
			ToolDescReplaceRules: []ToolDescReplaceRule{},
		}
	}
	out := FilterConfig{SystemInjection: cfg.Filter.SystemInjection}
	out.SystemReplaceRules = append([]SystemPromptReplaceRule(nil), cfg.Filter.SystemReplaceRules...)
	out.ToolDescReplaceRules = append([]ToolDescReplaceRule(nil), cfg.Filter.ToolDescReplaceRules...)
	if out.SystemReplaceRules == nil {
		out.SystemReplaceRules = []SystemPromptReplaceRule{}
	}
	if out.ToolDescReplaceRules == nil {
		out.ToolDescReplaceRules = []ToolDescReplaceRule{}
	}
	return out
}

// UpdateFilterConfig saves the filter configuration atomically.
func UpdateFilterConfig(fc FilterConfig) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return fmt.Errorf("config not initialized")
	}
	stored := FilterConfig{SystemInjection: fc.SystemInjection}
	stored.SystemReplaceRules = append([]SystemPromptReplaceRule(nil), fc.SystemReplaceRules...)
	stored.ToolDescReplaceRules = append([]ToolDescReplaceRule(nil), fc.ToolDescReplaceRules...)
	cfg.Filter = &stored
	return Save()
}

// GetModerationConfig returns a deep copy of the current moderation gateway
// configuration.
func GetModerationConfig() ModerationConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.Moderation == nil {
		return ModerationConfig{Rules: []JudgeRule{}}
	}
	out := ModerationConfig{
		Enabled:            cfg.Moderation.Enabled,
		JudgeModel:         cfg.Moderation.JudgeModel,
		ForwardURL:         cfg.Moderation.ForwardURL,
		ForwardKey:         cfg.Moderation.ForwardKey,
		ForwardFullContent: cfg.Moderation.ForwardFullContent,
	}
	out.Rules = append([]JudgeRule(nil), cfg.Moderation.Rules...)
	if out.Rules == nil {
		out.Rules = []JudgeRule{}
	}
	return out
}

// validateModerationConfig enforces the save-time invariant: an enabled gateway
// must have all three required fields filled and a syntactically valid http(s)
// ForwardURL. This keeps a residual/half-filled config from reaching disk. The
// runtime path additionally guards via ModerationReady (defense in depth against
// a hand-edited config.json).
func validateModerationConfig(mc ModerationConfig) error {
	if !mc.Enabled {
		return nil // disabled: fields may be blank
	}
	if strings.TrimSpace(mc.JudgeModel) == "" {
		return fmt.Errorf("judgeModel is required when moderation is enabled")
	}
	if strings.TrimSpace(mc.ForwardKey) == "" {
		return fmt.Errorf("forwardKey is required when moderation is enabled")
	}
	raw := strings.TrimSpace(mc.ForwardURL)
	if raw == "" {
		return fmt.Errorf("forwardUrl is required when moderation is enabled")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("forwardUrl must be a valid http(s) URL")
	}
	return nil
}

// UpdateModerationConfig validates and saves the moderation gateway config
// atomically. Returns an error (surfaced as HTTP 400 by the admin API) when an
// enabled config is missing required fields, so a half-configured gateway never
// lands on disk.
func UpdateModerationConfig(mc ModerationConfig) error {
	if err := validateModerationConfig(mc); err != nil {
		return err
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return fmt.Errorf("config not initialized")
	}
	stored := ModerationConfig{
		Enabled:            mc.Enabled,
		JudgeModel:         strings.TrimSpace(mc.JudgeModel),
		ForwardURL:         strings.TrimSpace(mc.ForwardURL),
		ForwardKey:         mc.ForwardKey,
		ForwardFullContent: mc.ForwardFullContent,
	}
	stored.Rules = append([]JudgeRule(nil), mc.Rules...)
	cfg.Moderation = &stored
	return Save()
}

// ThinkingConfig holds settings for AI thinking/reasoning mode.
// When enabled, models output their reasoning process alongside the response.
type ThinkingConfig struct {
	Suffix       string `json:"suffix"`       // Model name suffix that triggers thinking mode
	OpenAIFormat string `json:"openaiFormat"` // Output format for OpenAI-compatible responses
	ClaudeFormat string `json:"claudeFormat"` // Output format for Claude-compatible responses
}

// GetThinkingConfig 获取 thinking 配置
func GetThinkingConfig() ThinkingConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	suffix := cfg.ThinkingSuffix
	if suffix == "" {
		suffix = "-thinking"
	}
	openaiFormat := cfg.OpenAIThinkingFormat
	if openaiFormat == "" {
		openaiFormat = "reasoning_content"
	}
	claudeFormat := cfg.ClaudeThinkingFormat
	if claudeFormat == "" {
		claudeFormat = "thinking"
	}

	return ThinkingConfig{
		Suffix:       suffix,
		OpenAIFormat: openaiFormat,
		ClaudeFormat: claudeFormat,
	}
}

// UpdateThinkingConfig 更新 thinking 配置
func UpdateThinkingConfig(suffix, openaiFormat, claudeFormat string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ThinkingSuffix = suffix
	cfg.OpenAIThinkingFormat = openaiFormat
	cfg.ClaudeThinkingFormat = claudeFormat
	return Save()
}

// GetPreferredEndpoint 获取首选端点配置
func GetPreferredEndpoint() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.PreferredEndpoint == "" {
		return "auto"
	}
	return cfg.PreferredEndpoint
}

// UpdatePreferredEndpoint 更新首选端点配置
func UpdatePreferredEndpoint(endpoint string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.PreferredEndpoint = endpoint
	return Save()
}

// GetEndpointFallback returns whether endpoint fallback is enabled. Defaults to true.
func GetEndpointFallback() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.EndpointFallback == nil {
		return true
	}
	return *cfg.EndpointFallback
}

// UpdateEndpointFallback sets the endpoint fallback switch and persists the change.
func UpdateEndpointFallback(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.EndpointFallback = &enabled
	return Save()
}

// GetProxyURL 获取出站代理地址
func GetProxyURL() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.ProxyURL
}

// UpdateProxySettings 更新出站代理配置
func UpdateProxySettings(proxyURL string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ProxyURL = proxyURL
	return Save()
}

// GetAllowOverUsage returns whether over-usage is allowed when account quota is exhausted.
func GetAllowOverUsage() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.AllowOverUsage
}

// UpdateAllowOverUsage sets the over-usage setting and persists the change.
func UpdateAllowOverUsage(allow bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.AllowOverUsage = allow
	return Save()
}

// GetToolLeakFix returns whether the tool-call XML leak filter is enabled.
// Defaults to true. The KIRO_TOOL_LEAK_FIX env var overrides config.json when
// set: "off" disables, any other value enables.
func GetToolLeakFix() bool {
	if env := strings.TrimSpace(os.Getenv("KIRO_TOOL_LEAK_FIX")); env != "" {
		return strings.ToLower(env) != "off"
	}
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.ToolLeakFix == nil {
		return true
	}
	return *cfg.ToolLeakFix
}

// UpdateToolLeakFix sets the tool-call XML leak filter switch and persists it.
func UpdateToolLeakFix(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ToolLeakFix = &enabled
	return Save()
}

// GetPreserveToolHistory returns whether structured tool calls/results in
// history are preserved intact (true) or flattened to text (false). Defaults to
// true. The KIRO_PRESERVE_TOOL_HISTORY env var overrides config.json when set:
// "off" forces the conservative flatten fallback, any other value enables
// preserve. Use the fallback if the upstream is observed to reject requests
// carrying multiple structured tool pairs.
func GetPreserveToolHistory() bool {
	if env := strings.TrimSpace(os.Getenv("KIRO_PRESERVE_TOOL_HISTORY")); env != "" {
		return strings.ToLower(env) != "off"
	}
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.PreserveToolHistory == nil {
		return true
	}
	return *cfg.PreserveToolHistory
}

// UpdatePreserveToolHistory sets the structured-tool-history switch and persists it.
func UpdatePreserveToolHistory(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.PreserveToolHistory = &enabled
	return Save()
}

// GetToolLeakDebug returns whether verbose logging for the leak filter is on.
// Defaults to false. KIRO_TOOL_LEAK_DEBUG=1 forces it on regardless of config.
func GetToolLeakDebug() bool {
	if os.Getenv("KIRO_TOOL_LEAK_DEBUG") == "1" {
		return true
	}
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.ToolLeakDebug
}

// GetCreditsToUSD returns the conversion constant used to translate upstream
// credits into a target USD amount for usage calibration. Defaults to 0.2 when
// unset (0 or negative values fall back to the default).
func GetCreditsToUSD() float64 {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.CreditsToUSD <= 0 {
		return defaultCreditsToUSD
	}
	return cfg.CreditsToUSD
}

// UpdateCreditsToUSD sets the credit-to-USD conversion constant and persists the change.
func UpdateCreditsToUSD(v float64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.CreditsToUSD = v
	return Save()
}

// GetCacheReadBias returns the fraction of inferred cache_creation tokens that
// the calibrator should shift into cache_read before solving for the scale.
// Clamped to [0, 1); out-of-range or unset values yield 0 (no shift).
func GetCacheReadBias() float64 {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return 0
	}
	b := cfg.CacheReadBias
	if b <= 0 {
		return 0
	}
	if b >= 1 {
		return 0.999
	}
	return b
}

// UpdateCacheReadBias sets the cache-read display-bias factor and persists the
// change. Provided for completeness/test use; the value is intentionally not
// exposed in the admin UI (see field doc on Config.CacheReadBias).
func UpdateCacheReadBias(v float64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.CacheReadBias = v
	return Save()
}

// GetPricingURLs returns the upstream source URLs for the model pricing table
// (hash URL and pricing JSON URL). Cold-start configs are seeded with the
// built-in defaults; for older configs that predate the pricing block (or have
// an individual URL blank), each missing value falls back to its built-in
// default so the background updater always has a working source.
func GetPricingURLs() (hashURL, pricingURL string) {
	cfgLock.RLock()
	if cfg != nil && cfg.Pricing != nil {
		hashURL = cfg.Pricing.HashURL
		pricingURL = cfg.Pricing.PricingURL
	}
	cfgLock.RUnlock()
	if hashURL == "" {
		hashURL = DefaultPricingHashURL
	}
	if pricingURL == "" {
		pricingURL = DefaultPricingURL
	}
	return hashURL, pricingURL
}

// GetLogLevel returns the configured log level (debug/info/warn/error). Defaults to "info".
func GetLogLevel() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.LogLevel == "" {
		return "info"
	}
	return cfg.LogLevel
}

// UpdateLogLevel updates the log level setting and persists the change.
func UpdateLogLevel(level string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.LogLevel = level
	return Save()
}

// GetTimezone returns the configured *time.Location for date-based aggregation.
// Defaults to Asia/Shanghai if unset or invalid.
func GetTimezone() *time.Location {
	cfgLock.RLock()
	tz := ""
	if cfg != nil {
		tz = cfg.Timezone
	}
	cfgLock.RUnlock()
	if tz == "" {
		tz = "Asia/Shanghai"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}

// GetDataDir returns the directory containing the active config file.
// All auxiliary data files (state, daily stats, etc.) are co-located here.
func GetDataDir() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfgPath == "" {
		return "data"
	}
	return filepath.Dir(cfgPath)
}

type KiroClientConfig struct {
	KiroVersion   string
	SystemVersion string
	NodeVersion   string
}

func GetKiroClientConfig() KiroClientConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	kiroVersion := "0.11.107"
	if cfg != nil && cfg.KiroVersion != "" {
		kiroVersion = cfg.KiroVersion
	}

	systemVersion := ""
	if cfg != nil {
		systemVersion = cfg.SystemVersion
	}
	if systemVersion == "" {
		systemVersion = defaultSystemVersion()
	}

	nodeVersion := "22.22.0"
	if cfg != nil && cfg.NodeVersion != "" {
		nodeVersion = cfg.NodeVersion
	}

	return KiroClientConfig{
		KiroVersion:   kiroVersion,
		SystemVersion: systemVersion,
		NodeVersion:   nodeVersion,
	}
}

func defaultSystemVersion() string {
	switch runtime.GOOS {
	case "windows":
		return "win32#10.0.22631"
	case "darwin":
		return "darwin#24.6.0"
	default:
		return "linux#6.6.87"
	}
}
