package proxy

import (
	"math"
	"testing"
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

func TestCalibrateScaledUsageHitsTarget(t *testing.T) {
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

	// Choose credits so the target sits comfortably above the fixed input cost,
	// forcing a real (positive) scale factor.
	credits := 1.0
	target := credits * 0.2 // default CreditsToUSD when config is uninitialized

	gotOutput, gotUsage, applied := calibrateScaledUsage(model, credits, billedInput, output, usage)
	if !applied {
		t.Fatalf("expected calibration to apply")
	}

	// output is fixed — only cache fields are scaled.
	if gotOutput != output {
		t.Fatalf("output must not change: got %d, want %d", gotOutput, output)
	}

	// input_tokens is never passed into or mutated by the calibrator; the billed
	// input used for cost must remain the same value we provided.
	costAfter := totalListPriceCost(pricing, billedInput, gotOutput, gotUsage)
	// Tolerance: a few tokens worth of rounding error across four scaled fields.
	tol := 10 * pricing.OutputCostPerToken
	if math.Abs(costAfter-target) > tol {
		t.Fatalf("calibrated cost $%.6f not within $%.6f of target $%.6f", costAfter, tol, target)
	}

	// cache_creation must equal the sum of its 5m/1h breakdown after scaling.
	if gotUsage.CacheCreationInputTokens != gotUsage.CacheCreation5mInputTokens+gotUsage.CacheCreation1hInputTokens {
		t.Fatalf("cache_creation %d != 5m %d + 1h %d",
			gotUsage.CacheCreationInputTokens, gotUsage.CacheCreation5mInputTokens, gotUsage.CacheCreation1hInputTokens)
	}
}

func TestCalibrateScaledUsageScaleDown(t *testing.T) {
	model := "claude-sonnet-4.5"
	usage := promptCacheUsage{
		CacheReadInputTokens:       4000,
		CacheCreation5mInputTokens: 2000,
		CacheCreationInputTokens:   2000,
	}
	output := 1000

	// Small credits => target below the unscaled variable cost but still above
	// the fixed cost => scale in (0,1).
	credits := 0.1
	gotOutput, gotUsage, applied := calibrateScaledUsage(model, credits, 500, output, usage)
	if !applied {
		t.Fatalf("expected calibration to apply for scale-down case")
	}
	// output is now fixed — only cache fields are scaled.
	if gotOutput != output {
		t.Fatalf("output must not change: got %d, want %d", gotOutput, output)
	}
	if gotUsage.CacheReadInputTokens >= usage.CacheReadInputTokens {
		t.Fatalf("expected cache_read to shrink, got %d (was %d)", gotUsage.CacheReadInputTokens, usage.CacheReadInputTokens)
	}
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

	t.Run("nothing to scale", func(t *testing.T) {
		// No output and no cache activity => variable cost is zero.
		empty := promptCacheUsage{}
		_, _, applied := calibrateScaledUsage("claude-sonnet-4.5", 1.0, 1000, 0, empty)
		if applied {
			t.Fatalf("expected skip when there is nothing to scale")
		}
	})

	t.Run("target below fixed input cost", func(t *testing.T) {
		// Huge input, tiny credits => target dollar amount is far below the fixed
		// input list-price cost, so scale would be <= 0.
		_, _, applied := calibrateScaledUsage("claude-sonnet-4.5", 0.0001, 5_000_000, 500, usage)
		if applied {
			t.Fatalf("expected skip when target is below fixed input cost")
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
