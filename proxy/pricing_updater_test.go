package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"kiro-go/config"
)

func mustMarshalIndent(t *testing.T, v interface{}) []byte {
	t.Helper()
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

const sampleUpstreamPricing = `{
  "claude-opus-4-8": {
    "litellm_provider": "anthropic",
    "input_cost_per_token": 5e-06,
    "output_cost_per_token": 2.5e-05,
    "cache_read_input_token_cost": 5e-07,
    "cache_creation_input_token_cost": 6.25e-06,
    "cache_creation_input_token_cost_above_1hr": 1e-05,
    "max_input_tokens": 1000000,
    "mode": "chat"
  },
  "gemini-2.0-flash": {
    "litellm_provider": "vertex_ai-language-models",
    "input_cost_per_token": 1e-07,
    "output_cost_per_token": 4e-07
  },
  "some-anthropic-embedding": {
    "litellm_provider": "anthropic",
    "input_cost_per_token": 0,
    "output_cost_per_token": 0,
    "mode": "embedding"
  }
}`

func TestRemapSourcePricingFiltersToAnthropicWithCost(t *testing.T) {
	table, err := remapSourcePricing([]byte(sampleUpstreamPricing))
	if err != nil {
		t.Fatalf("remapSourcePricing error: %v", err)
	}

	if len(table) != 1 {
		t.Fatalf("expected exactly 1 retained model, got %d: %+v", len(table), table)
	}

	p, ok := table["claude-opus-4-8"]
	if !ok {
		t.Fatalf("expected claude-opus-4-8 to be retained")
	}
	if p.InputCostPerToken != 5e-06 || p.OutputCostPerToken != 2.5e-05 {
		t.Fatalf("unexpected costs mapped: %+v", p)
	}
	if p.CacheReadInputTokenCost != 5e-07 || p.CacheCreationInputTokenCost != 6.25e-06 || p.CacheCreation1hInputTokenCost != 1e-05 {
		t.Fatalf("unexpected cache costs mapped: %+v", p)
	}

	if _, ok := table["gemini-2.0-flash"]; ok {
		t.Fatalf("non-anthropic model should be filtered out")
	}
	if _, ok := table["some-anthropic-embedding"]; ok {
		t.Fatalf("anthropic model without cost should be filtered out")
	}
}

func TestRemapSourcePricingRejectsBadJSON(t *testing.T) {
	if _, err := remapSourcePricing([]byte("not json")); err == nil {
		t.Fatalf("expected error on malformed JSON")
	}
}

func TestParseModelPricingTableEmptyOnBadJSON(t *testing.T) {
	table := parseModelPricingTable([]byte("{ broken"))
	if len(table) != 0 {
		t.Fatalf("expected empty table on bad JSON, got %d entries", len(table))
	}
}

func TestSetModelPricingTableIgnoresNil(t *testing.T) {
	// Snapshot the active table, then confirm a nil set does not clobber it.
	before, ok := lookupModelPricing("claude-opus-4.8")
	if !ok {
		t.Fatalf("expected seed table to contain claude-opus-4.8")
	}
	setModelPricingTable(nil)
	after, ok := lookupModelPricing("claude-opus-4.8")
	if !ok || after != before {
		t.Fatalf("nil set must not alter the active table")
	}
}

func TestPricingPathsUnderConfigDir(t *testing.T) {
	jsonPath, hashPath := pricingPaths()
	if filepath.Base(jsonPath) != pricingFileName {
		t.Fatalf("json path base = %q, want %q", filepath.Base(jsonPath), pricingFileName)
	}
	if filepath.Base(hashPath) != pricingHashFileName {
		t.Fatalf("hash path base = %q, want %q", filepath.Base(hashPath), pricingHashFileName)
	}
	if filepath.Dir(jsonPath) != filepath.Dir(hashPath) {
		t.Fatalf("json and hash files should live in the same directory")
	}
}

// TestRemappedTableRoundTrips confirms a remapped table serializes to the same
// schema parseModelPricingTable expects, so an updater write is loadable on the
// next start.
func TestRemappedTableRoundTrips(t *testing.T) {
	table, err := remapSourcePricing([]byte(sampleUpstreamPricing))
	if err != nil {
		t.Fatalf("remap error: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, pricingFileName)

	raw := mustMarshalIndent(t, table)
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatalf("write temp pricing: %v", err)
	}

	loaded := parseModelPricingTable(raw)
	if len(loaded) != len(table) {
		t.Fatalf("round-trip size mismatch: %d != %d", len(loaded), len(table))
	}
	if loaded["claude-opus-4-8"] != table["claude-opus-4-8"] {
		t.Fatalf("round-trip value mismatch for claude-opus-4-8")
	}
}

func TestPricingURLsFallBackToDefaults(t *testing.T) {
	// With no config loaded, GetPricingURLs must fall back to the built-in
	// defaults rather than returning empty strings, so the updater always has a
	// concrete source to fetch from.
	hashURL, jsonURL := config.GetPricingURLs()
	if hashURL != config.DefaultPricingHashURL {
		t.Fatalf("hash URL = %q, want default %q", hashURL, config.DefaultPricingHashURL)
	}
	if jsonURL != config.DefaultPricingURL {
		t.Fatalf("json URL = %q, want default %q", jsonURL, config.DefaultPricingURL)
	}
}
