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

// TestClassifyDownstreamErrorAllBranches covers nil, quota/429, and generic
// error paths in addition to the 400 path tested above.
func TestClassifyDownstreamErrorAllBranches(t *testing.T) {
	tests := []struct {
		name           string
		err            error
		wantStatus     int
		wantMsg        string
		wantCountFail  bool
	}{
		{
			name:          "nil error returns 503",
			err:           nil,
			wantStatus:    503,
			wantMsg:       msgServiceUnavailable,
			wantCountFail: false,
		},
		{
			name:          "quota 429 error",
			err:           errors.New("HTTP 429: quota exceeded"),
			wantStatus:    429,
			wantMsg:       msgServiceCoolingDown,
			wantCountFail: true,
		},
		{
			name:          "generic 500 error",
			err:           errors.New("HTTP 500: internal server error"),
			wantStatus:    503,
			wantMsg:       msgServiceUnavailable,
			wantCountFail: true,
		},
		{
			name:          "all endpoints failed",
			err:           errors.New("all endpoints failed"),
			wantStatus:    503,
			wantMsg:       msgServiceUnavailable,
			wantCountFail: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, msg, countFail := classifyDownstreamError(tc.err)
			if status != tc.wantStatus {
				t.Errorf("status: got %d, want %d", status, tc.wantStatus)
			}
			if msg != tc.wantMsg {
				t.Errorf("msg: got %q, want %q", msg, tc.wantMsg)
			}
			if countFail != tc.wantCountFail {
				t.Errorf("countAsFailure: got %v, want %v", countFail, tc.wantCountFail)
			}
		})
	}
}

// TestDownstreamErrorMessage covers the downstreamErrorMessage helper.
func TestDownstreamErrorMessage(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{nil, msgServiceUnavailable},
		{errors.New("HTTP 429: quota"), msgServiceCoolingDown},
		{errors.New(`HTTP 400: {"message":"Bedrock error message: bad input"}`), `HTTP 400: {"message":"bad input"}`},
		{errors.New("random failure"), msgServiceUnavailable},
	}
	for _, tc := range tests {
		got := downstreamErrorMessage(tc.err)
		if got != tc.want {
			t.Errorf("downstreamErrorMessage(%v) = %q, want %q", tc.err, got, tc.want)
		}
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
