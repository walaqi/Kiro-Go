package proxy

import (
	"math"
	"path/filepath"
	"testing"

	"kiro-go/config"
)

// totalListPriceCost computes the published list-price dollar cost of a reported
// usage set (with input_tokens held fixed) for assertion in calibration tests.
func totalListPriceCost(p modelPricing, billedInput, output int, usage promptCacheUsage) float64 {
	return float64(billedInput)*p.InputCostPerToken +
		float64(output)*p.OutputCostPerToken +
		float64(usage.CacheReadInputTokens)*p.CacheReadInputTokenCost +
		float64(usage.CacheCreation5mInputTokens)*p.CacheCreationInputTokenCost +
		float64(usage.CacheCreation1hInputTokens)*p.CacheCreation1hInputTokenCost
}

// conservedCacheTotal returns C = cache_read + cache_creation(top-level) for
// conservation assertions. C uses the top-level CacheCreationInputTokens — the
// same field billedClaudeInputTokens subtracts — NOT the 5m/1h breakdown sum,
// which is not guaranteed equal to it (see calibrateScaledUsage).
func conservedCacheTotal(u promptCacheUsage) int {
	return u.CacheReadInputTokens + u.CacheCreationInputTokens
}

func TestLookupModelPricing(t *testing.T) {
	cases := []struct {
		name  string
		model string
		want  bool
	}{
		{"dash form direct", "claude-opus-4-8", true},
		{"dot form", "claude-opus-4.8", true},
		{"thinking suffix on dot form", "claude-opus-4.8-thinking", true},
		{"thinking suffix on dash form", "claude-sonnet-4-5-thinking", true},
		{"sonnet dot form", "claude-sonnet-4.5", true},
		{"haiku dot form", "claude-haiku-4.5", true},
		{"legacy alias claude-3-5-sonnet", "claude-3-5-sonnet", true},
		{"uppercase", "CLAUDE-OPUS-4.8", true},
		{"unknown model", "gemini-2.0-flash", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := lookupModelPricing(tc.model)
			if ok != tc.want {
				t.Fatalf("lookupModelPricing(%q) ok = %v, want %v", tc.model, ok, tc.want)
			}
		})
	}
}

// TestCalibrateSplitHitsTarget verifies that when the dollar budget V falls
// inside the achievable band, the split is solved so the reported cost matches
// the target exactly, while the total cache token count is conserved.
func TestCalibrateSplitHitsTarget(t *testing.T) {
	model := "claude-sonnet-4.5"
	pricing, ok := lookupModelPricing(model)
	if !ok {
		t.Fatalf("expected pricing for %s", model)
	}

	billedInput := 1000
	output := 500
	usage := promptCacheUsage{
		CacheReadInputTokens:       2000,
		CacheCreation5mInputTokens: 1000,
		CacheCreation1hInputTokens: 200,
		CacheCreationInputTokens:   1200,
	}
	C := conservedCacheTotal(usage)

	// Pick credits so the residual budget V lands strictly inside the band
	// [C*p_cr, C*p_creation_blended], forcing a genuine interior split.
	pBlended := (1000*pricing.CacheCreationInputTokenCost + 200*pricing.CacheCreation1hInputTokenCost) / 1200
	fixed := float64(billedInput)*pricing.InputCostPerToken + float64(output)*pricing.OutputCostPerToken
	midV := float64(C) * (pricing.CacheReadInputTokenCost + pBlended) / 2 // band midpoint
	credits := (fixed + midV) / config.GetCreditsToUSD()

	gotOutput, gotUsage, applied := calibrateScaledUsage(model, credits, billedInput, output, usage)
	if !applied {
		t.Fatalf("expected calibration to apply")
	}
	if gotOutput != output {
		t.Fatalf("output must not change: got %d, want %d", gotOutput, output)
	}

	// Token conservation: the total cache token count is held fixed.
	if got := conservedCacheTotal(gotUsage); got != C {
		t.Fatalf("cache token conservation violated: got C=%d, want %d", got, C)
	}

	// cache_creation must equal the sum of its 5m/1h breakdown.
	if gotUsage.CacheCreationInputTokens != gotUsage.CacheCreation5mInputTokens+gotUsage.CacheCreation1hInputTokens {
		t.Fatalf("cache_creation %d != 5m %d + 1h %d",
			gotUsage.CacheCreationInputTokens, gotUsage.CacheCreation5mInputTokens, gotUsage.CacheCreation1hInputTokens)
	}

	// Dollar alignment: reported cost matches the target within rounding noise.
	target := credits * config.GetCreditsToUSD()
	costAfter := totalListPriceCost(pricing, billedInput, gotOutput, gotUsage)
	tol := 2 * pricing.CacheCreationInputTokenCost // a couple tokens of rounding
	if math.Abs(costAfter-target) > tol {
		t.Fatalf("calibrated cost $%.6f not within $%.6f of target $%.6f", costAfter, tol, target)
	}
}

// TestCalibrateSplitClampLow verifies that when the budget V is below the
// all-read floor, the split clamps to all cache_read (cheapest) and conserves
// the total. Dollar alignment is intentionally sacrificed in this out-of-band
// case to preserve a physically valid token distribution.
func TestCalibrateSplitClampLow(t *testing.T) {
	model := "claude-sonnet-4.5"
	pricing, ok := lookupModelPricing(model)
	if !ok {
		t.Fatalf("expected pricing for %s", model)
	}

	billedInput := 500
	output := 1000
	usage := promptCacheUsage{
		CacheReadInputTokens:       4000,
		CacheCreation5mInputTokens: 2000,
		CacheCreationInputTokens:   2000,
	}
	C := conservedCacheTotal(usage)

	// Tiny credits => V below C*p_cr => clamp to all-read.
	credits := 0.05
	_, gotUsage, applied := calibrateScaledUsage(model, credits, billedInput, output, usage)
	if !applied {
		t.Fatalf("expected calibration to apply for clamp-low case")
	}
	if gotUsage.CacheReadInputTokens != C {
		t.Fatalf("expected all tokens routed to cache_read: got read=%d, want %d", gotUsage.CacheReadInputTokens, C)
	}
	if gotUsage.CacheCreationInputTokens != 0 {
		t.Fatalf("expected zero cache_creation under clamp-low, got %d", gotUsage.CacheCreationInputTokens)
	}
	if got := conservedCacheTotal(gotUsage); got != C {
		t.Fatalf("conservation violated under clamp: got %d, want %d", got, C)
	}
	_ = pricing
}

// TestCalibrateSplitClampHigh verifies that when V exceeds the all-creation
// ceiling, the split clamps to all cache_creation (most expensive) and conserves
// the total.
func TestCalibrateSplitClampHigh(t *testing.T) {
	model := "claude-sonnet-4.5"
	pricing, ok := lookupModelPricing(model)
	if !ok {
		t.Fatalf("expected pricing for %s", model)
	}

	billedInput := 100
	output := 100
	usage := promptCacheUsage{
		CacheReadInputTokens:       3000,
		CacheCreation5mInputTokens: 1000,
		CacheCreationInputTokens:   1000,
	}
	C := conservedCacheTotal(usage)

	// Choose credits so V > C*p_creation (all-creation ceiling).
	fixed := float64(billedInput)*pricing.InputCostPerToken + float64(output)*pricing.OutputCostPerToken
	overV := float64(C) * pricing.CacheCreationInputTokenCost * 1.5
	credits := (fixed + overV) / config.GetCreditsToUSD()

	_, gotUsage, applied := calibrateScaledUsage(model, credits, billedInput, output, usage)
	if !applied {
		t.Fatalf("expected calibration to apply for clamp-high case")
	}
	if gotUsage.CacheReadInputTokens != 0 {
		t.Fatalf("expected zero cache_read under clamp-high, got %d", gotUsage.CacheReadInputTokens)
	}
	if got := conservedCacheTotal(gotUsage); got != C {
		t.Fatalf("conservation violated under clamp-high: got %d, want %d", got, C)
	}
	// All creation, split should preserve the original 5m:1h ratio (here all 5m).
	if gotUsage.CacheCreation5mInputTokens != C {
		t.Fatalf("expected all creation in 5m bucket, got 5m=%d 1h=%d", gotUsage.CacheCreation5mInputTokens, gotUsage.CacheCreation1hInputTokens)
	}
}

// TestCalibrateSplitConservesAlways sweeps a range of credit values and asserts
// that token conservation (read + cache_creation(top-level) == C) holds for
// every applied calibration, interior or clamped.
func TestCalibrateSplitConservesAlways(t *testing.T) {
	model := "claude-sonnet-4.5"
	usage := promptCacheUsage{
		CacheReadInputTokens:       2500,
		CacheCreation5mInputTokens: 1500,
		CacheCreation1hInputTokens: 500,
		CacheCreationInputTokens:   2000,
	}
	C := conservedCacheTotal(usage)
	for _, credits := range []float64{0.01, 0.05, 0.1, 0.2, 0.5, 1.0, 2.0, 5.0} {
		_, gotUsage, applied := calibrateScaledUsage(model, credits, 800, 600, usage)
		if !applied {
			continue
		}
		if got := conservedCacheTotal(gotUsage); got != C {
			t.Fatalf("conservation violated at credits=%.2f: got %d, want %d", credits, got, C)
		}
		if gotUsage.CacheCreationInputTokens != gotUsage.CacheCreation5mInputTokens+gotUsage.CacheCreation1hInputTokens {
			t.Fatalf("creation breakdown inconsistent at credits=%.2f", credits)
		}
	}
}

// TestCalibrateSplitTopLevelCreationIsTheConservedTotal guards the C-source
// invariant: the conserved total must come from the top-level
// CacheCreationInputTokens (the field billedClaudeInputTokens subtracts), NOT
// the 5m/1h breakdown sum. cache_tracker.go can emit usage where the top-level
// creation is zeroed (min-cacheable threshold) or capped (85% rule) while the
// TTL breakdown stays nonzero. Conserving against the breakdown sum would
// fabricate cache tokens that break billedInput + read + creation ==
// estimatedInput.
func TestCalibrateSplitTopLevelCreationIsTheConservedTotal(t *testing.T) {
	model := "claude-sonnet-4.5"

	t.Run("zeroed top-level creation with nonzero breakdown", func(t *testing.T) {
		// Top-level creation forced to 0 by the min-cacheable threshold, but the
		// TTL breakdown still reports 900. No read either => C == 0 => skip.
		// Calibration must NOT invent 900 creation tokens here.
		usage := promptCacheUsage{
			CacheCreationInputTokens:   0,
			CacheReadInputTokens:       0,
			CacheCreation5mInputTokens: 900,
			CacheCreation1hInputTokens: 0,
		}
		_, gotUsage, applied := calibrateScaledUsage(model, 1.0, 1000, 500, usage)
		if applied {
			t.Fatalf("expected skip: C derives from top-level creation (0)+read (0)=0, got applied with %+v", gotUsage)
		}
		if gotUsage != usage {
			t.Fatalf("usage must be returned unchanged on skip, got %+v", gotUsage)
		}
	})

	t.Run("capped top-level creation below breakdown sum", func(t *testing.T) {
		// Top-level creation capped to 1000 while the breakdown sums to 1500.
		// C must be read(500)+1000=1500, conserving against the top-level total,
		// NOT read+breakdown(500+1500=2000).
		usage := promptCacheUsage{
			CacheCreationInputTokens:   1000,
			CacheReadInputTokens:       500,
			CacheCreation5mInputTokens: 1200,
			CacheCreation1hInputTokens: 300,
		}
		wantC := usage.CacheReadInputTokens + usage.CacheCreationInputTokens // 1500
		_, gotUsage, applied := calibrateScaledUsage(model, 1.0, 800, 400, usage)
		if !applied {
			t.Fatalf("expected calibration to apply")
		}
		if got := conservedCacheTotal(gotUsage); got != wantC {
			t.Fatalf("C must conserve against top-level creation: got %d, want %d", got, wantC)
		}
		if gotUsage.CacheReadInputTokens+gotUsage.CacheCreationInputTokens != wantC {
			t.Fatalf("read+creation must equal C=%d, got read=%d creation=%d",
				wantC, gotUsage.CacheReadInputTokens, gotUsage.CacheCreationInputTokens)
		}
	})
}

func TestCalibrateScaledUsageSkips(t *testing.T) {
	usage := promptCacheUsage{
		CacheReadInputTokens:       2000,
		CacheCreation5mInputTokens: 1000,
		CacheCreationInputTokens:   1000,
	}

	t.Run("zero credits", func(t *testing.T) {
		gotOutput, gotUsage, applied := calibrateScaledUsage("claude-sonnet-4.5", 0, 1000, 500, usage)
		if applied {
			t.Fatalf("expected skip when credits are zero")
		}
		if gotOutput != 500 || gotUsage != usage {
			t.Fatalf("expected inputs returned unchanged")
		}
	})

	t.Run("negative credits", func(t *testing.T) {
		_, _, applied := calibrateScaledUsage("claude-sonnet-4.5", -5, 1000, 500, usage)
		if applied {
			t.Fatalf("expected skip when credits are negative")
		}
	})

	t.Run("unknown model", func(t *testing.T) {
		gotOutput, gotUsage, applied := calibrateScaledUsage("gemini-2.0-flash", 1.0, 1000, 500, usage)
		if applied {
			t.Fatalf("expected skip for unknown model")
		}
		if gotOutput != 500 || gotUsage != usage {
			t.Fatalf("expected inputs returned unchanged for unknown model")
		}
	})

	t.Run("no cache activity", func(t *testing.T) {
		// No cache tokens => C == 0 => nothing to split.
		empty := promptCacheUsage{}
		_, _, applied := calibrateScaledUsage("claude-sonnet-4.5", 1.0, 1000, 0, empty)
		if applied {
			t.Fatalf("expected skip when there is no cache activity")
		}
	})
}

func TestCalibrateScaledUsageDoesNotMutateInputUsage(t *testing.T) {
	usage := promptCacheUsage{
		CacheReadInputTokens:       2000,
		CacheCreation5mInputTokens: 1000,
		CacheCreation1hInputTokens: 200,
		CacheCreationInputTokens:   1200,
	}
	snapshot := usage
	_, _, _ = calibrateScaledUsage("claude-sonnet-4.5", 1.0, 1000, 500, usage)
	if usage != snapshot {
		t.Fatalf("calibrateScaledUsage mutated its input usage: %+v != %+v", usage, snapshot)
	}
}

// TestCacheReadBiasIsNoOp confirms the legacy CacheReadBias knob no longer
// influences calibration: identical inputs must yield identical usage
// regardless of the configured bias value. (Round-trip of the setting itself is
// covered by TestSettingsCacheReadBiasRoundTrip in handler_test.go.)
func TestCacheReadBiasIsNoOp(t *testing.T) {
	dir := t.TempDir()
	if err := config.Init(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	t.Cleanup(func() { _ = config.UpdateCacheReadBias(0) })

	model := "claude-sonnet-4.5"
	billedInput := 1000
	output := 500
	usage := promptCacheUsage{
		CacheReadInputTokens:       2000,
		CacheCreation5mInputTokens: 1000,
		CacheCreation1hInputTokens: 200,
		CacheCreationInputTokens:   1200,
	}
	credits := 1.0

	_, baseline, baseApplied := calibrateScaledUsage(model, credits, billedInput, output, usage)
	if !baseApplied {
		t.Fatalf("baseline calibration must apply")
	}

	if err := config.UpdateCacheReadBias(0.6); err != nil {
		t.Fatalf("UpdateCacheReadBias: %v", err)
	}
	_, biased, biasedApplied := calibrateScaledUsage(model, credits, billedInput, output, usage)
	if !biasedApplied {
		t.Fatalf("calibration must apply with bias set")
	}

	if biased != baseline {
		t.Fatalf("CacheReadBias must be a no-op: baseline=%+v biased=%+v", baseline, biased)
	}
}
