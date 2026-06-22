package proxy

import "testing"

// TestIsMalformedRequestErrorMessage verifies that the upstream HTTP 400
// "Improperly formed request." rejection is classified as a permanent
// request-structure error (so callers fail fast and return 400 to the client
// instead of rotating accounts/endpoints and burning credits), while transient
// or account-specific errors are NOT misclassified.
func TestIsMalformedRequestErrorMessage(t *testing.T) {
	malformed := []string{
		`HTTP 400: {"message":"Improperly formed request.","reason":null}`,
		"Improperly formed request.",
		"http 400 something",
	}
	for _, m := range malformed {
		if !isMalformedRequestErrorMessage(m) {
			t.Errorf("expected malformed classification for %q", m)
		}
	}

	notMalformed := []string{
		`HTTP 500: {"message":"Encountered an unexpected error..."}`,
		`HTTP 500: {"message":"...MODEL_TEMPORARILY_UNAVAILABLE"}`,
		"HTTP 429 quota exceeded",
		"HTTP 403 forbidden",
		"HTTP 401 unauthorized",
		"no available kiro profile",
		"all endpoints failed",
		"",
	}
	for _, m := range notMalformed {
		if isMalformedRequestErrorMessage(m) {
			t.Errorf("did NOT expect malformed classification for %q", m)
		}
	}
}
