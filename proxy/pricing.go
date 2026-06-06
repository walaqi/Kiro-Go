package proxy

import (
	_ "embed"
	"encoding/json"
	"math"
	"strings"

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

//go:embed model_pricing.json
var modelPricingRaw []byte

var modelPricingTable = loadModelPricingTable()

func loadModelPricingTable() map[string]modelPricing {
	table := make(map[string]modelPricing)
	if err := json.Unmarshal(modelPricingRaw, &table); err != nil {
		// Embedded data is generated at build time; a parse failure means the
		// embedded file is malformed. Fall back to an empty table so calibration
		// degrades to a no-op rather than crashing the proxy.
		logger.Errorf("failed to parse embedded model_pricing.json: %v", err)
		return map[string]modelPricing{}
	}
	return table
}

// lookupModelPricing resolves a model name (in the proxy's internal dot form,
// possibly carrying a thinking suffix) to its list prices. It normalizes the
// name to the dash form used as keys in the pricing table and falls back to the
// configured model aliases when there is no direct hit.
func lookupModelPricing(model string) (modelPricing, bool) {
	for _, candidate := range pricingLookupCandidates(model) {
		if p, ok := modelPricingTable[candidate]; ok {
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

// calibrateScaledUsage rescales the variable usage components (output,
// cache_read, cache_creation) so that the total list-price cost of the reported
// usage matches the dollar value implied by the upstream credits, while leaving
// the billed input_tokens untouched to avoid disturbing client compaction
// signals.
//
// The fixed cost is billedInput * input_price. The variable cost at list price
// is output*p_out + cache_read*p_cr + cc5m*p_5m + cc1h*p_1h. We solve for a
// single scale factor s such that:
//
//	fixedCost + s * variableCost = credits * CreditsToUSD
//
// then apply s to output and the three cache components. Calibration is skipped
// (returning the inputs unchanged with applied=false) when credits are absent,
// the model price is unknown, there is nothing to scale, or the target dollar
// amount cannot be reached without driving the variable components to zero or
// negative.
func calibrateScaledUsage(model string, credits float64, billedInput, output int, usage promptCacheUsage) (int, promptCacheUsage, bool) {
	if credits <= 0 {
		return output, usage, false
	}

	pricing, ok := lookupModelPricing(model)
	if !ok {
		return output, usage, false
	}

	variableCost := float64(output)*pricing.OutputCostPerToken +
		float64(usage.CacheReadInputTokens)*pricing.CacheReadInputTokenCost +
		float64(usage.CacheCreation5mInputTokens)*pricing.CacheCreationInputTokenCost +
		float64(usage.CacheCreation1hInputTokens)*pricing.CacheCreation1hInputTokenCost
	if variableCost <= 0 {
		// Nothing to scale (no output and no cache activity).
		return output, usage, false
	}

	target := credits * config.GetCreditsToUSD()
	fixedCost := float64(billedInput) * pricing.InputCostPerToken
	scale := (target - fixedCost) / variableCost
	if scale <= 0 {
		// The fixed input cost alone already meets or exceeds the target; scaling
		// the variable parts would zero them out or go negative. Leave the
		// heuristic values untouched.
		logger.Warnf("credit calibration skipped for model %s: target $%.6f below fixed input cost $%.6f (credits=%.4f)", model, target, fixedCost, credits)
		return output, usage, false
	}

	scaledOutput := scaleToken(output, scale)
	scaledRead := scaleToken(usage.CacheReadInputTokens, scale)
	scaled5m := scaleToken(usage.CacheCreation5mInputTokens, scale)
	scaled1h := scaleToken(usage.CacheCreation1hInputTokens, scale)

	calibrated := usage
	calibrated.CacheReadInputTokens = scaledRead
	calibrated.CacheCreation5mInputTokens = scaled5m
	calibrated.CacheCreation1hInputTokens = scaled1h
	calibrated.CacheCreationInputTokens = scaled5m + scaled1h

	return scaledOutput, calibrated, true
}

// scaleToken multiplies a token count by the scale factor and rounds to the
// nearest non-negative integer.
func scaleToken(tokens int, scale float64) int {
	if tokens <= 0 {
		return 0
	}
	scaled := int(math.Round(float64(tokens) * scale))
	if scaled < 0 {
		return 0
	}
	return scaled
}
