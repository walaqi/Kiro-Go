package proxy

import (
	_ "embed"
	"encoding/json"
	"math"
	"strings"
	"sync"

	"kiro-go/config"
	"kiro-go/logger"
)

// modelPricing holds the per-token list prices (in USD) used to calibrate the
// reported usage numbers against published model rates. Only the fields needed
// for calibration are retained from the upstream pricing table.
type modelPricing struct {
	InputCostPerToken             float64 `json:"input_cost_per_token"`
	OutputCostPerToken            float64 `json:"output_cost_per_token"`
	CacheReadInputTokenCost       float64 `json:"cache_read_input_token_cost"`
	CacheCreationInputTokenCost   float64 `json:"cache_creation_input_token_cost"`
	CacheCreation1hInputTokenCost float64 `json:"cache_creation_input_token_cost_above_1hr"`
}

// modelPricingSeed is the embedded fallback pricing table. The authoritative
// copy lives on disk at data/model_pricing.json and is refreshed periodically
// by the background updater (see pricing_updater.go). The seed is used when no
// on-disk copy exists yet — e.g. on a fresh deployment before the first fetch
// succeeds — so calibration still works out of the box.
//
//go:embed model_pricing.json
var modelPricingSeed []byte

var (
	modelPricingMu    sync.RWMutex
	modelPricingTable = parseModelPricingTable(modelPricingSeed)
)

// parseModelPricingTable unmarshals a raw model_pricing.json document into the
// calibration table. On parse failure it returns an empty table so calibration
// degrades to a no-op rather than crashing the proxy.
func parseModelPricingTable(raw []byte) map[string]modelPricing {
	table := make(map[string]modelPricing)
	if err := json.Unmarshal(raw, &table); err != nil {
		logger.Errorf("failed to parse model_pricing.json: %v", err)
		return map[string]modelPricing{}
	}
	return table
}

// setModelPricingTable atomically replaces the active pricing table. It is
// called by the background updater after a successful fetch and remap.
func setModelPricingTable(table map[string]modelPricing) {
	if table == nil {
		return
	}
	modelPricingMu.Lock()
	modelPricingTable = table
	modelPricingMu.Unlock()
}

// lookupModelPricing resolves a model name (in the proxy's internal dot form,
// possibly carrying a thinking suffix) to its list prices. It normalizes the
// name to the dash form used as keys in the pricing table and falls back to the
// configured model aliases when there is no direct hit.
func lookupModelPricing(model string) (modelPricing, bool) {
	modelPricingMu.RLock()
	table := modelPricingTable
	modelPricingMu.RUnlock()
	for _, candidate := range pricingLookupCandidates(model) {
		if p, ok := table[candidate]; ok {
			return p, true
		}
	}
	return modelPricing{}, false
}

// pricingLookupCandidates produces the ordered set of keys to try against the
// pricing table for a given model name.
func pricingLookupCandidates(model string) []string {
	lower := strings.ToLower(strings.TrimSpace(model))
	if lower == "" {
		return nil
	}

	// Strip a trailing "-thinking" (or any "-thinking..." variant) suffix; the
	// thinking flavor shares list prices with the base model.
	if idx := strings.Index(lower, "-thinking"); idx != -1 {
		lower = lower[:idx]
	}

	seen := make(map[string]bool)
	candidates := make([]string, 0, 4)
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		candidates = append(candidates, name)
	}

	// 1) As-is (handles dash-form keys like "claude-opus-4-8" directly).
	add(lower)

	// 2) Dot form converted to dash form: "claude-opus-4.8" -> "claude-opus-4-8".
	add(strings.ReplaceAll(lower, ".", "-"))

	// 3) Resolve through the proxy's model aliases (e.g. legacy claude-3-* names,
	//    dated snapshots) then convert that dot-form result to dash form.
	mapped := MapModel(lower)
	if mapped != "" {
		mappedLower := strings.ToLower(mapped)
		add(mappedLower)
		add(strings.ReplaceAll(mappedLower, ".", "-"))
	}

	return candidates
}

// calibrateScaledUsage redistributes the inferred prompt-cache tokens between
// the cache_read and cache_creation buckets so that the total list-price cost
// of the reported usage matches the dollar value implied by the upstream
// credits, while holding the *total* cache token count fixed (token
// conservation).
//
// Upstream Kiro reports only a single credits scalar, never token counts. Both
// input_tokens and output_tokens are held fixed (local tiktoken estimates).
// The total cache token count C = cache_read + cache_creation is also trusted —
// it reflects the observed context structure (estimatedInput - billedInput).
// The only genuinely uncertain quantity is how C splits between the cheap
// "read" bucket and the expensive "creation" buckets (this depends on cache
// timing that upstream never reveals), so that split is the single free
// variable. We solve it so the cache dollars equal the residual budget V:
//
//	read + cc = C                  (token conservation)
//	read*p_cr + cc*p_c = V          (dollar alignment)
//	  where V   = credits*CreditsToUSD - billedInput*p_in - output*p_out
//	        p_c = creation price blended by the observed cc5m:cc1h ratio
//
//	=> cc = (V - C*p_cr) / (p_c - p_cr),  read = C - cc
//
// When V falls outside the achievable band [C*p_cr, C*p_c] the split is clamped
// to the nearest boundary (all-read or all-creation). We preserve token
// conservation in preference to exact dollar alignment: fabricating an
// impossible token distribution is worse for a Claude-compatible usage report
// than a small dollar mismatch on an out-of-band request. The cc5m:cc1h ratio
// is preserved across the redistribution.
//
// (CacheReadBias is intentionally NOT consulted: under split calibration the
// read/creation ratio is fully determined by C and V, leaving no free degree
// for a display-bias knob. The config field is retained as a legacy no-op.)
//
// Calibration is skipped (applied=false, inputs returned unchanged) when:
//   - credits <= 0
//   - model pricing is unknown
//   - C <= 0 (no cache activity — nothing to split)
//   - the pricing table is degenerate (creation price <= read price)
func calibrateScaledUsage(model string, credits float64, billedInput, output int, usage promptCacheUsage) (int, promptCacheUsage, bool) {
	if credits <= 0 {
		return output, usage, false
	}

	pricing, ok := lookupModelPricing(model)
	if !ok {
		return output, usage, false
	}

	// The conserved total C must use the SAME creation field the caller used to
	// derive billedInput (billedClaudeInputTokens subtracts the top-level
	// CacheCreationInputTokens, not the 5m/1h breakdown sum). The breakdown and
	// the top-level total are NOT guaranteed equal: the min-cacheable threshold
	// can zero the top-level creation while the TTL breakdown stays nonzero, and
	// the 85% cap can shrink the top-level creation below the breakdown sum (see
	// cache_tracker.go). Conserving against the breakdown sum would satisfy
	// read+cc5m+cc1h==breakdown but break the client-visible invariant
	// billedInput + read + cache_creation == estimatedInput. So C is built from
	// the reported creation total; the 5m/1h fields are used ONLY as a ratio
	// hint for re-splitting the solved creation amount.
	cc5mOrig := usage.CacheCreation5mInputTokens
	cc1hOrig := usage.CacheCreation1hInputTokens
	ratioDenom := cc5mOrig + cc1hOrig // breakdown sum — split-ratio hint only
	ccTotalReported := usage.CacheCreationInputTokens
	total := usage.CacheReadInputTokens + ccTotalReported // C
	if total <= 0 {
		// No cache activity — nothing to split.
		return output, usage, false
	}

	// Creation price blended by the observed 5m:1h ratio. When no breakdown is
	// available, any creation derived below is routed to the 5m bucket, so use
	// the 5m creation price as the blended creation price.
	pRead := pricing.CacheReadInputTokenCost
	var pCreation float64
	if ratioDenom > 0 {
		pCreation = (float64(cc5mOrig)*pricing.CacheCreationInputTokenCost +
			float64(cc1hOrig)*pricing.CacheCreation1hInputTokenCost) / float64(ratioDenom)
	} else {
		pCreation = pricing.CacheCreationInputTokenCost
	}
	if pCreation <= pRead {
		// Degenerate pricing — cannot differentiate read from creation.
		return output, usage, false
	}

	C := float64(total)
	V := credits*config.GetCreditsToUSD() -
		float64(billedInput)*pricing.InputCostPerToken -
		float64(output)*pricing.OutputCostPerToken

	// Solve the split, clamping to the achievable band [C*pRead, C*pCreation].
	var ccFloat float64
	switch {
	case V <= C*pRead:
		ccFloat = 0 // V below all-read cost: clamp to all read.
		logger.Debugf("credit calibration clamped to all-read for model %s: V=$%.6f <= floor $%.6f (credits=%.4f)", model, V, C*pRead, credits)
	case V >= C*pCreation:
		ccFloat = C // V above all-creation cost: clamp to all creation.
		logger.Debugf("credit calibration clamped to all-creation for model %s: V=$%.6f >= ceil $%.6f (credits=%.4f)", model, V, C*pCreation, credits)
	default:
		ccFloat = (V - C*pRead) / (pCreation - pRead)
	}

	// Round into integers while preserving conservation: cache_read absorbs the
	// rounding remainder so read + cache_creation(top-level) == total exactly
	// (the top-level creation is reset to cc5m+cc1h below).
	ccTotal := int(math.Round(ccFloat))
	if ccTotal < 0 {
		ccTotal = 0
	} else if ccTotal > total {
		ccTotal = total
	}
	read := total - ccTotal

	// Re-split the solved creation total across 5m/1h preserving the observed
	// breakdown ratio (or all to 5m when no breakdown is available).
	var cc5m, cc1h int
	if ccTotal > 0 {
		if ratioDenom > 0 {
			cc5m = int(math.Round(float64(ccTotal) * float64(cc5mOrig) / float64(ratioDenom)))
			if cc5m > ccTotal {
				cc5m = ccTotal
			}
			cc1h = ccTotal - cc5m
		} else {
			cc5m = ccTotal
		}
	}

	calibrated := usage
	calibrated.CacheReadInputTokens = read
	calibrated.CacheCreation5mInputTokens = cc5m
	calibrated.CacheCreation1hInputTokens = cc1h
	calibrated.CacheCreationInputTokens = cc5m + cc1h

	return output, calibrated, true
}
