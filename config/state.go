package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// This file implements the runtime-state store. Frequently-mutated counters
// (per-request account / api-key usage and process-wide totals) are persisted to
// a sibling "state" file instead of config.json, so that:
//   - config.json (which holds credentials / refresh tokens) is not rewritten on
//     every request, avoiding write amplification and corruption risk;
//   - stat writes never block readers of the main config on a disk write.
//
// The stat fields still live on the in-memory Account / ApiKeyEntry / Config
// structs (tagged json:"-" so they are excluded from config.json). Those fields
// are the authoritative in-memory copy read by the admin panel and pool seeding.
// state.json is purely their persisted mirror: it is *derived from cfg* at flush
// time (see persistState) rather than maintained as a parallel structure. That
// has two consequences worth noting:
//   - There is no separate delete bookkeeping. Removing an account / API key from
//     cfg and flushing automatically drops its counters from state.json.
//   - Concurrent flushes cannot persist stale data: persistState is serialized by
//     flushLock and each run snapshots the latest cfg, so the final on-disk file
//     always reflects the most recent in-memory counters.
//
// On startup the file is read back into the in-memory fields; a one-time
// migration seeds state.json from a legacy config.json that still carried the
// counters inline.

// AccountState holds the per-account runtime counters persisted to state.json.
type AccountState struct {
	RequestCount int     `json:"requestCount,omitempty"`
	ErrorCount   int     `json:"errorCount,omitempty"`
	LastUsed     int64   `json:"lastUsed,omitempty"`
	TotalTokens  int     `json:"totalTokens,omitempty"`
	TotalCredits float64 `json:"totalCredits,omitempty"`
}

// ApiKeyState holds the per-API-key usage counters persisted to state.json.
type ApiKeyState struct {
	TokensUsed    int64   `json:"tokensUsed,omitempty"`
	CreditsUsed   float64 `json:"creditsUsed,omitempty"`
	RequestsCount int64   `json:"requestsCount,omitempty"`
	LastUsedAt    int64   `json:"lastUsedAt,omitempty"`
}

// GlobalStats holds the process-wide request / token totals persisted to state.json.
type GlobalStats struct {
	TotalRequests   int     `json:"totalRequests,omitempty"`
	SuccessRequests int     `json:"successRequests,omitempty"`
	FailedRequests  int     `json:"failedRequests,omitempty"`
	TotalTokens     int     `json:"totalTokens,omitempty"`
	TotalCredits    float64 `json:"totalCredits,omitempty"`
}

// stateData is the on-disk shape of state.json.
type stateData struct {
	Global   GlobalStats             `json:"global"`
	Accounts map[string]AccountState `json:"accounts"`
	ApiKeys  map[string]ApiKeyState  `json:"apiKeys"`
}

var (
	statePath string
	// flushLock serializes persistState so concurrent flushes cannot reorder
	// their writes; the last flush to acquire it always carries the latest
	// snapshot of cfg. It is acquired only inside persistState, which then takes
	// cfgLock.RLock — so the lock order is always flushLock → cfgLock and a
	// cfgLock holder must never call persistState (it would self-deadlock on the
	// RWMutex). All stat writers therefore release cfgLock before flushing.
	flushLock sync.Mutex
)

func (s AccountState) isZero() bool { return s == AccountState{} }
func (s ApiKeyState) isZero() bool  { return s == ApiKeyState{} }

// deriveStatePath returns the sibling state-file path for a given config path.
// "data/config.json" -> "data/config.state.json"; a path without extension gets
// ".state" appended. Each config (and thus each multi-instance deployment) gets
// its own isolated state file.
func deriveStatePath(cfgPath string) string {
	if cfgPath == "" {
		return ""
	}
	ext := filepath.Ext(cfgPath)
	if ext != "" {
		return cfgPath[:len(cfgPath)-len(ext)] + ".state" + ext
	}
	return cfgPath + ".state"
}

// buildStateFromConfigLocked snapshots the current counters off cfg into the
// on-disk shape. The caller MUST hold at least cfgLock.RLock. Zero-valued
// entries are omitted to keep state.json compact.
func buildStateFromConfigLocked() stateData {
	s := stateData{
		Accounts: map[string]AccountState{},
		ApiKeys:  map[string]ApiKeyState{},
	}
	if cfg == nil {
		return s
	}
	s.Global = GlobalStats{
		TotalRequests:   cfg.TotalRequests,
		SuccessRequests: cfg.SuccessRequests,
		FailedRequests:  cfg.FailedRequests,
		TotalTokens:     cfg.TotalTokens,
		TotalCredits:    cfg.TotalCredits,
	}
	for i := range cfg.Accounts {
		a := &cfg.Accounts[i]
		st := AccountState{
			RequestCount: a.RequestCount,
			ErrorCount:   a.ErrorCount,
			LastUsed:     a.LastUsed,
			TotalTokens:  a.TotalTokens,
			TotalCredits: a.TotalCredits,
		}
		if !st.isZero() {
			s.Accounts[a.ID] = st
		}
	}
	for i := range cfg.ApiKeys {
		k := &cfg.ApiKeys[i]
		st := ApiKeyState{
			TokensUsed:    k.TokensUsed,
			CreditsUsed:   k.CreditsUsed,
			RequestsCount: k.RequestsCount,
			LastUsedAt:    k.LastUsedAt,
		}
		if !st.isZero() {
			s.ApiKeys[k.ID] = st
		}
	}
	return s
}

// persistState atomically writes the current counters (snapshotted from cfg) to
// state.json. It is safe to call concurrently: flushLock serializes the snapshot
// + write so the on-disk file always ends up matching the most recent cfg state.
// The caller MUST NOT hold cfgLock (persistState takes cfgLock.RLock itself).
// A blank statePath (uninitialized) is a no-op.
func persistState() error {
	flushLock.Lock()
	defer flushLock.Unlock()
	if statePath == "" {
		return nil
	}
	cfgLock.RLock()
	s := buildStateFromConfigLocked()
	cfgLock.RUnlock()
	data, err := json.MarshalIndent(&s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(statePath, data, 0600)
}

// persistStateLocked is persistState for callers that already hold cfgLock
// (read or write). It snapshots cfg without re-locking, then writes outside any
// further cfg locking. flushLock still serializes the actual writes. NOTE: this
// briefly holds flushLock while doing disk IO under the caller's cfgLock, so use
// it only on cold paths (load-time migration), never on the request hot path.
func persistStateLocked() error {
	flushLock.Lock()
	defer flushLock.Unlock()
	if statePath == "" {
		return nil
	}
	s := buildStateFromConfigLocked()
	data, err := json.MarshalIndent(&s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(statePath, data, 0600)
}

// legacyConfigStats mirrors the stat fields that older config.json files carried
// inline, used only for the one-time migration into state.json.
type legacyConfigStats struct {
	TotalRequests   int     `json:"totalRequests"`
	SuccessRequests int     `json:"successRequests"`
	FailedRequests  int     `json:"failedRequests"`
	TotalTokens     int     `json:"totalTokens"`
	TotalCredits    float64 `json:"totalCredits"`
	Accounts        []struct {
		ID           string  `json:"id"`
		RequestCount int     `json:"requestCount"`
		ErrorCount   int     `json:"errorCount"`
		LastUsed     int64   `json:"lastUsed"`
		TotalTokens  int     `json:"totalTokens"`
		TotalCredits float64 `json:"totalCredits"`
	} `json:"accounts"`
	ApiKeys []struct {
		ID            string  `json:"id"`
		LastUsedAt    int64   `json:"lastUsedAt"`
		TokensUsed    int64   `json:"tokensUsed"`
		CreditsUsed   float64 `json:"creditsUsed"`
		RequestsCount int64   `json:"requestsCount"`
	} `json:"apiKeys"`
}

// loadStateLocked reads state.json into the in-memory cfg counters. If the file
// does not exist, it seeds state from legacyConfigBytes (the raw config.json that
// may still carry inline counters) so upgrading deployments keep their statistics,
// then persists a fresh state.json. The caller MUST hold cfgLock (write).
// Returns migrated=true when a legacy config was the source, signalling the
// caller to rewrite config.json so the now-orphaned inline counters are dropped.
func loadStateLocked(legacyConfigBytes []byte) (migrated bool, err error) {
	if statePath != "" {
		data, readErr := os.ReadFile(statePath)
		if readErr == nil {
			var s stateData
			if err := json.Unmarshal(data, &s); err != nil {
				return false, err
			}
			applyStateLocked(s)
			return false, nil
		}
		if !os.IsNotExist(readErr) {
			return false, readErr
		}
	}

	// state.json absent: seed from any inline counters in the legacy config bytes.
	migratedAny := false
	if len(legacyConfigBytes) > 0 {
		var legacy legacyConfigStats
		if err := json.Unmarshal(legacyConfigBytes, &legacy); err == nil {
			s := stateData{
				Global: GlobalStats{
					TotalRequests:   legacy.TotalRequests,
					SuccessRequests: legacy.SuccessRequests,
					FailedRequests:  legacy.FailedRequests,
					TotalTokens:     legacy.TotalTokens,
					TotalCredits:    legacy.TotalCredits,
				},
				Accounts: map[string]AccountState{},
				ApiKeys:  map[string]ApiKeyState{},
			}
			for _, a := range legacy.Accounts {
				if a.ID == "" {
					continue
				}
				st := AccountState{
					RequestCount: a.RequestCount,
					ErrorCount:   a.ErrorCount,
					LastUsed:     a.LastUsed,
					TotalTokens:  a.TotalTokens,
					TotalCredits: a.TotalCredits,
				}
				if !st.isZero() {
					s.Accounts[a.ID] = st
					migratedAny = true
				}
			}
			for _, k := range legacy.ApiKeys {
				if k.ID == "" {
					continue
				}
				st := ApiKeyState{
					TokensUsed:    k.TokensUsed,
					CreditsUsed:   k.CreditsUsed,
					RequestsCount: k.RequestsCount,
					LastUsedAt:    k.LastUsedAt,
				}
				if !st.isZero() {
					s.ApiKeys[k.ID] = st
					migratedAny = true
				}
			}
			if s.Global != (GlobalStats{}) {
				migratedAny = true
			}
			applyStateLocked(s)
		}
	}
	// Write the initial state.json (empty on a fresh install, or seeded on upgrade).
	if err := persistStateLocked(); err != nil {
		return false, err
	}
	return migratedAny, nil
}

// applyStateLocked copies persisted counters into the live in-memory cfg structs
// so readers (admin panel, pool Reload seeding) observe the restored statistics.
// The caller MUST hold cfgLock (write).
func applyStateLocked(s stateData) {
	if cfg == nil {
		return
	}
	cfg.TotalRequests = s.Global.TotalRequests
	cfg.SuccessRequests = s.Global.SuccessRequests
	cfg.FailedRequests = s.Global.FailedRequests
	cfg.TotalTokens = s.Global.TotalTokens
	cfg.TotalCredits = s.Global.TotalCredits
	for i := range cfg.Accounts {
		if st, ok := s.Accounts[cfg.Accounts[i].ID]; ok {
			cfg.Accounts[i].RequestCount = st.RequestCount
			cfg.Accounts[i].ErrorCount = st.ErrorCount
			cfg.Accounts[i].LastUsed = st.LastUsed
			cfg.Accounts[i].TotalTokens = st.TotalTokens
			cfg.Accounts[i].TotalCredits = st.TotalCredits
		}
	}
	for i := range cfg.ApiKeys {
		if st, ok := s.ApiKeys[cfg.ApiKeys[i].ID]; ok {
			cfg.ApiKeys[i].TokensUsed = st.TokensUsed
			cfg.ApiKeys[i].CreditsUsed = st.CreditsUsed
			cfg.ApiKeys[i].RequestsCount = st.RequestsCount
			cfg.ApiKeys[i].LastUsedAt = st.LastUsedAt
		}
	}
}
