package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

const (
	// pricingUpdateInterval is how often the background updater checks the
	// upstream hash for changes.
	pricingUpdateInterval = 24 * time.Hour

	// pricingFileName is the on-disk name of the calibration pricing table,
	// stored alongside config.json in the data directory.
	pricingFileName = "model_pricing.json"

	// pricingHashFileName stores the upstream sha256 of the last successfully
	// applied source document, used to skip redundant downloads.
	pricingHashFileName = "model_pricing.json.sha256"
)

// sourceModelEntry is the subset of an upstream pricing entry we care about.
// The upstream document carries many provider-specific fields; only anthropic
// chat models with the cost fields below are retained for calibration.
type sourceModelEntry struct {
	LitellmProvider               string  `json:"litellm_provider"`
	InputCostPerToken             float64 `json:"input_cost_per_token"`
	OutputCostPerToken            float64 `json:"output_cost_per_token"`
	CacheReadInputTokenCost       float64 `json:"cache_read_input_token_cost"`
	CacheCreationInputTokenCost   float64 `json:"cache_creation_input_token_cost"`
	CacheCreation1hInputTokenCost float64 `json:"cache_creation_input_token_cost_above_1hr"`
}

// pricingPaths resolves the on-disk locations of the pricing table and its
// companion hash file, both living in the configuration data directory.
func pricingPaths() (jsonPath, hashPath string) {
	dir := config.GetConfigDir()
	return filepath.Join(dir, pricingFileName), filepath.Join(dir, pricingHashFileName)
}

// startPricingUpdater loads the on-disk pricing table (seeding it from the
// embedded copy when absent), then launches the background refresh loop. It is
// safe to call once during handler construction.
func startPricingUpdater(stop <-chan struct{}) {
	loadPricingFromDiskOrSeed()
	go pricingUpdateLoop(stop)
}

// loadPricingFromDiskOrSeed loads data/model_pricing.json into the active
// table. If the file does not exist, the embedded seed is written to disk and
// kept as the active table so calibration works on a fresh deployment.
func loadPricingFromDiskOrSeed() {
	jsonPath, _ := pricingPaths()

	raw, err := os.ReadFile(jsonPath)
	if err == nil {
		table := parseModelPricingTable(raw)
		if len(table) > 0 {
			setModelPricingTable(table)
			logger.Infof("[Pricing] Loaded %d models from %s", len(table), jsonPath)
			return
		}
		logger.Warnf("[Pricing] On-disk %s parsed to an empty table; falling back to embedded seed", jsonPath)
	} else if !os.IsNotExist(err) {
		logger.Warnf("[Pricing] Failed to read %s: %v; falling back to embedded seed", jsonPath, err)
	}

	// Seed the on-disk copy from the embedded fallback so future starts (and the
	// updater's hash comparison) have a concrete file to work with.
	if writeErr := os.WriteFile(jsonPath, modelPricingSeed, 0644); writeErr != nil {
		logger.Warnf("[Pricing] Failed to write seed pricing file %s: %v", jsonPath, writeErr)
	} else {
		logger.Infof("[Pricing] Seeded %s from embedded pricing table", jsonPath)
	}
	// The active table is already the parsed seed (set at package init), so no
	// further action is needed for calibration to function.
}

// pricingUpdateLoop checks the upstream hash on startup and then every
// pricingUpdateInterval, refreshing the on-disk table when the hash changes.
func pricingUpdateLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(pricingUpdateInterval)
	defer ticker.Stop()

	// Initial check shortly after startup, staggered behind account refresh.
	time.Sleep(20 * time.Second)
	if err := checkAndUpdatePricing(); err != nil {
		logger.Warnf("[Pricing] Initial update check failed: %v", err)
	}

	for {
		select {
		case <-ticker.C:
			if err := checkAndUpdatePricing(); err != nil {
				logger.Warnf("[Pricing] Update check failed: %v", err)
			}
		case <-stop:
			return
		}
	}
}

// checkAndUpdatePricing fetches the upstream hash, compares it to the locally
// stored hash, and if changed downloads the source document, remaps it to the
// calibration schema, writes it to disk, and swaps the active table.
func checkAndUpdatePricing() error {
	jsonPath, hashPath := pricingPaths()
	// config.GetPricingURLs falls back to the built-in defaults
	// (config.DefaultPricingHashURL / DefaultPricingURL) when unconfigured, so
	// these are never empty.
	hashURL, jsonURL := config.GetPricingURLs()

	remoteHash, err := fetchPricingText(hashURL)
	if err != nil {
		return fmt.Errorf("fetch upstream hash: %w", err)
	}
	remoteHash = strings.TrimSpace(remoteHash)
	if remoteHash == "" {
		return fmt.Errorf("upstream hash is empty")
	}

	if localHash, readErr := os.ReadFile(hashPath); readErr == nil {
		if strings.TrimSpace(string(localHash)) == remoteHash {
			logger.Debugf("[Pricing] Upstream hash unchanged (%s); skipping update", shortHash(remoteHash))
			return nil
		}
	}

	logger.Infof("[Pricing] Upstream hash changed (%s); fetching new pricing table", shortHash(remoteHash))

	rawJSON, err := fetchPricingBytes(jsonURL)
	if err != nil {
		return fmt.Errorf("fetch upstream pricing json: %w", err)
	}

	table, err := remapSourcePricing(rawJSON)
	if err != nil {
		return fmt.Errorf("remap upstream pricing: %w", err)
	}
	if len(table) == 0 {
		return fmt.Errorf("remapped pricing table is empty; keeping existing table")
	}

	mapped, err := json.MarshalIndent(table, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal mapped pricing: %w", err)
	}

	if err := os.WriteFile(jsonPath, mapped, 0644); err != nil {
		return fmt.Errorf("write %s: %w", jsonPath, err)
	}
	// Persist the hash only after the table write succeeded, so a failed write
	// is retried on the next cycle rather than masked by a matching hash.
	if err := os.WriteFile(hashPath, []byte(remoteHash+"\n"), 0644); err != nil {
		logger.Warnf("[Pricing] Failed to write hash file %s: %v", hashPath, err)
	}

	setModelPricingTable(table)
	logger.Infof("[Pricing] Updated pricing table: %d models (hash %s)", len(table), shortHash(remoteHash))
	return nil
}

// remapSourcePricing parses the upstream LiteLLM-style document and produces the
// trimmed calibration table, retaining only anthropic chat models that carry an
// input/output per-token cost.
func remapSourcePricing(raw []byte) (map[string]modelPricing, error) {
	var source map[string]sourceModelEntry
	if err := json.Unmarshal(raw, &source); err != nil {
		return nil, err
	}

	table := make(map[string]modelPricing)
	for name, entry := range source {
		if !strings.EqualFold(entry.LitellmProvider, "anthropic") {
			continue
		}
		if entry.InputCostPerToken <= 0 && entry.OutputCostPerToken <= 0 {
			continue
		}
		table[name] = modelPricing{
			InputCostPerToken:             entry.InputCostPerToken,
			OutputCostPerToken:            entry.OutputCostPerToken,
			CacheReadInputTokenCost:       entry.CacheReadInputTokenCost,
			CacheCreationInputTokenCost:   entry.CacheCreationInputTokenCost,
			CacheCreation1hInputTokenCost: entry.CacheCreation1hInputTokenCost,
		}
	}
	return table, nil
}

// fetchPricingBytes downloads a URL using the proxy's configured outbound HTTP
// client (honoring the global proxy setting).
func fetchPricingBytes(url string) ([]byte, error) {
	client := GetRestClientForProxy(config.GetProxyURL())
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	// Cap the read to a generous bound to avoid unbounded memory use.
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

func fetchPricingText(url string) (string, error) {
	b, err := fetchPricingBytes(url)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
