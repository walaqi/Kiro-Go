package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// readStateFile decodes the sibling state.json for assertions.
func readStateFile(t *testing.T, cfgFile string) stateData {
	t.Helper()
	raw, err := os.ReadFile(deriveStatePath(cfgFile))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var s stateData
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode state file: %v", err)
	}
	return s
}

func TestDeriveStatePath(t *testing.T) {
	cases := map[string]string{
		"data/config.json":  "data/config.state.json",
		"/etc/kiro/cfg.json": "/etc/kiro/cfg.state.json",
		"config":            "config.state",
		"":                  "",
	}
	for in, want := range cases {
		if got := deriveStatePath(in); got != want {
			t.Errorf("deriveStatePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestStatsPersistToStateNotConfig verifies that hot-path counters land in
// state.json and are NOT written back into config.json.
func TestStatsPersistToStateNotConfig(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}

	acc := Account{ID: "acc-1", Email: "a@example.com", Enabled: true}
	if err := AddAccount(acc); err != nil {
		t.Fatalf("add account: %v", err)
	}

	if err := UpdateAccountStats("acc-1", 5, 1, 1234, 9.5, 1700000000); err != nil {
		t.Fatalf("update account stats: %v", err)
	}
	if err := UpdateStats(10, 8, 2, 4321, 12.0); err != nil {
		t.Fatalf("update global stats: %v", err)
	}

	// state.json should carry the counters.
	st := readStateFile(t, cfgFile)
	if st.Accounts["acc-1"].TotalTokens != 1234 || st.Accounts["acc-1"].TotalCredits != 9.5 {
		t.Fatalf("account state not persisted: %+v", st.Accounts["acc-1"])
	}
	if st.Global.TotalRequests != 10 || st.Global.TotalTokens != 4321 {
		t.Fatalf("global state not persisted: %+v", st.Global)
	}

	// config.json must NOT contain the stat fields (json:"-").
	rawCfg, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var probe map[string]interface{}
	if err := json.Unmarshal(rawCfg, &probe); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	for _, k := range []string{"totalRequests", "totalTokens", "totalCredits", "successRequests"} {
		if _, ok := probe[k]; ok {
			t.Fatalf("config.json should not contain global stat %q", k)
		}
	}
	accs := probe["accounts"].([]interface{})
	a0 := accs[0].(map[string]interface{})
	for _, k := range []string{"requestCount", "totalTokens", "totalCredits", "lastUsed", "errorCount"} {
		if _, ok := a0[k]; ok {
			t.Fatalf("config.json account should not contain stat %q", k)
		}
	}
}

// TestStateRoundTripOnReload verifies counters survive a reload from state.json.
func TestStateRoundTripOnReload(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := AddAccount(Account{ID: "acc-1", Enabled: true}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := UpdateAccountStats("acc-1", 3, 0, 777, 4.2, 1700000000); err != nil {
		t.Fatalf("update stats: %v", err)
	}
	if err := UpdateStats(99, 90, 9, 5000, 50.0); err != nil {
		t.Fatalf("update global: %v", err)
	}

	// Simulate a process restart: reload from disk.
	if err := Init(cfgFile); err != nil {
		t.Fatalf("reload: %v", err)
	}
	accs := GetAccounts()
	if len(accs) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accs))
	}
	if accs[0].TotalTokens != 777 || accs[0].TotalCredits != 4.2 || accs[0].RequestCount != 3 {
		t.Fatalf("account counters not restored: %+v", accs[0])
	}
	tr, sr, fr, tt, tc := GetStats()
	if tr != 99 || sr != 90 || fr != 9 || tt != 5000 || tc != 50.0 {
		t.Fatalf("global stats not restored: %d %d %d %d %v", tr, sr, fr, tt, tc)
	}
}

// TestDeleteAccountDropsState is the explicit requirement: deleting an account
// must also remove its persisted runtime counters from state.json.
func TestDeleteAccountDropsState(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := AddAccount(Account{ID: "keep", Enabled: true}); err != nil {
		t.Fatalf("add keep: %v", err)
	}
	if err := AddAccount(Account{ID: "gone", Enabled: true}); err != nil {
		t.Fatalf("add gone: %v", err)
	}
	if err := UpdateAccountStats("keep", 1, 0, 10, 1.0, 1); err != nil {
		t.Fatalf("stats keep: %v", err)
	}
	if err := UpdateAccountStats("gone", 2, 0, 20, 2.0, 2); err != nil {
		t.Fatalf("stats gone: %v", err)
	}

	if _, ok := readStateFile(t, cfgFile).Accounts["gone"]; !ok {
		t.Fatalf("precondition: gone should have state before delete")
	}

	if err := DeleteAccount("gone"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	st := readStateFile(t, cfgFile)
	if _, ok := st.Accounts["gone"]; ok {
		t.Fatalf("deleted account's state should be dropped, got %+v", st.Accounts)
	}
	if _, ok := st.Accounts["keep"]; !ok {
		t.Fatalf("surviving account's state should remain")
	}
}

// TestDeleteApiKeyDropsState mirrors the account requirement for API keys.
func TestDeleteApiKeyDropsState(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	created, err := AddApiKey(ApiKeyEntry{Name: "k", Key: "sk-del", Enabled: true})
	if err != nil {
		t.Fatalf("add key: %v", err)
	}
	if err := RecordApiKeyUsage(created.ID, 100, 1.5); err != nil {
		t.Fatalf("record usage: %v", err)
	}
	if _, ok := readStateFile(t, cfgFile).ApiKeys[created.ID]; !ok {
		t.Fatalf("precondition: key should have state before delete")
	}

	if err := DeleteApiKey(created.ID); err != nil {
		t.Fatalf("delete key: %v", err)
	}
	if _, ok := readStateFile(t, cfgFile).ApiKeys[created.ID]; ok {
		t.Fatalf("deleted key's state should be dropped")
	}
}

// TestLegacyConfigStatsMigration verifies that a pre-split config.json carrying
// inline counters is migrated into state.json on first load, and the inline
// fields are stripped from config.json afterwards.
func TestLegacyConfigStatsMigration(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")

	seed := map[string]interface{}{
		"password":        "p",
		"port":            8080,
		"host":            "0.0.0.0",
		"totalRequests":   42,
		"totalTokens":     9000,
		"totalCredits":    7.5,
		"successRequests": 40,
		"failedRequests":  2,
		"accounts": []map[string]interface{}{
			{"id": "acc-legacy", "enabled": true, "requestCount": 11, "totalTokens": 333, "totalCredits": 3.3, "lastUsed": 1699999999},
		},
		"apiKeys": []map[string]interface{}{
			{"id": "key-legacy", "key": "sk-legacy", "enabled": true, "tokensUsed": 555, "creditsUsed": 5.5, "requestsCount": 7, "createdAt": 1},
		},
	}
	raw, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}

	// Counters should be visible in-memory.
	accs := GetAccounts()
	if len(accs) != 1 || accs[0].TotalTokens != 333 || accs[0].RequestCount != 11 {
		t.Fatalf("legacy account counters not migrated into memory: %+v", accs)
	}
	keys := ListApiKeys()
	if len(keys) != 1 || keys[0].TokensUsed != 555 || keys[0].RequestsCount != 7 {
		t.Fatalf("legacy api key counters not migrated: %+v", keys)
	}
	tr, _, _, tt, tc := GetStats()
	if tr != 42 || tt != 9000 || tc != 7.5 {
		t.Fatalf("legacy global stats not migrated: %d %d %v", tr, tt, tc)
	}

	// state.json should now hold them.
	st := readStateFile(t, cfgFile)
	if st.Global.TotalRequests != 42 || st.Accounts["acc-legacy"].TotalTokens != 333 || st.ApiKeys["key-legacy"].TokensUsed != 555 {
		t.Fatalf("state.json missing migrated counters: %+v", st)
	}

	// config.json should have been rewritten without the inline stat fields.
	rawCfg, _ := os.ReadFile(cfgFile)
	var probe map[string]interface{}
	if err := json.Unmarshal(rawCfg, &probe); err != nil {
		t.Fatalf("decode rewritten config: %v", err)
	}
	if _, ok := probe["totalRequests"]; ok {
		t.Fatalf("rewritten config.json should not carry totalRequests")
	}
	a0 := probe["accounts"].([]interface{})[0].(map[string]interface{})
	if _, ok := a0["totalTokens"]; ok {
		t.Fatalf("rewritten config.json account should not carry totalTokens")
	}
}
