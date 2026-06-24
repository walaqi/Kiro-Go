package proxy

import (
	"errors"
	"testing"
)

func TestSanitizeUpstreamError(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{
			input: `HTTP 400: {"message":"Bedrock error message: This model doesn't support the image content block that you provided. Update the content block and try again.","reason":"IMAGE_FORMAT_UNSUPPORTED"}`,
			want:  `HTTP 400: {"message":"This model doesn't support the image content block that you provided. Update the content block and try again.","reason":"IMAGE_FORMAT_UNSUPPORTED"}`,
		},
		{
			input: `HTTP 400: {"message":"bedrock error message: some other error","reason":null}`,
			want:  `HTTP 400: {"message":"some other error","reason":null}`,
		},
		{
			input: `HTTP 400: {"message":"Improperly formed request.","reason":null}`,
			want:  `HTTP 400: {"message":"Improperly formed request.","reason":null}`,
		},
		{
			input: "Bedrock throttling limit reached",
			want:  "throttling limit reached",
		},
		{
			input: "no bedrock references here",
			want:  "no references here",
		},
		{
			input: "",
			want:  "",
		},
	}

	for _, tc := range tests {
		got := sanitizeUpstreamError(tc.input)
		if got != tc.want {
			t.Errorf("sanitizeUpstreamError(%q)\n  got:  %q\n  want: %q", tc.input, got, tc.want)
		}
	}
}

// TestClassifyDownstreamErrorSanitizesBedrock verifies that the full
// classifyDownstreamError path strips "Bedrock" from 400 messages before
// they reach the client.
func TestClassifyDownstreamErrorSanitizesBedrock(t *testing.T) {
	err := errors.New(`HTTP 400: {"message":"Bedrock error message: This model doesn't support the image content block.","reason":"IMAGE_FORMAT_UNSUPPORTED"}`)
	status, msg, countAsFailure := classifyDownstreamError(err)

	if status != 400 {
		t.Fatalf("expected status 400, got %d", status)
	}
	if countAsFailure {
		t.Fatal("expected countAsFailure=false for 400")
	}
	if contains(msg, "Bedrock") || contains(msg, "bedrock") {
		t.Fatalf("downstream message still contains 'Bedrock': %s", msg)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && stringContains(s, substr)))
}

func stringContains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
