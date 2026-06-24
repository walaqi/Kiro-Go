package proxy

import (
	"kiro-go/config"
	"kiro-go/logger"
	"strings"
	"time"
)

const maxAccountRetryAttempts = 3

func isQuotaErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "429") || strings.Contains(msg, "quota")
}

func isOverageErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "402") && strings.Contains(msg, "overage")
}

func isSuspensionErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "temporarily_suspended") ||
		strings.Contains(msg, "temporarily is suspended") ||
		strings.Contains(msg, "account suspended")
}

func isProfileUnavailableErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "no available kiro profile")
}

func isAuthErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "http 401") ||
		strings.Contains(msg, "http 403") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "forbidden") ||
		strings.Contains(msg, "authentication failed") ||
		strings.Contains(msg, "token invalid") ||
		strings.Contains(msg, "token expired") ||
		strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "access token expired") ||
		strings.Contains(msg, "refresh token expired")
}

// isMalformedRequestErrorMessage reports whether the upstream rejected the
// request because of the request body itself (HTTP 400 "Improperly formed
// request."), not because of the account or endpoint. Such failures are
// permanent for the given payload: retrying on another account or endpoint
// cannot succeed and only burns upstream credits. Callers must fail the request
// immediately instead of rotating accounts.
func isMalformedRequestErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "http 400") ||
		strings.Contains(msg, "improperly formed request")
}

// sanitizeUpstreamError strips internal platform identifiers from an upstream
// error message body so they never reach downstream clients. The upstream may
// embed prefixes like "Bedrock error message: " inside the JSON payload which
// would reveal infrastructure details.
func sanitizeUpstreamError(msg string) string {
	// Case-insensitive strip of "Bedrock error message: " prefix that AWS
	// injects into the "message" field of 400 response bodies.
	lower := strings.ToLower(msg)
	for {
		idx := strings.Index(lower, "bedrock error message: ")
		if idx == -1 {
			break
		}
		prefix := msg[:idx]
		suffix := msg[idx+len("bedrock error message: "):]
		msg = prefix + suffix
		lower = strings.ToLower(msg)
	}
	// Also strip bare "Bedrock" if it appears as a standalone word (e.g.
	// "Bedrock throttling" edge cases). Only strip when surrounded by word
	// boundaries (space/punctuation/start/end) to avoid false positives.
	msg = strings.ReplaceAll(msg, "Bedrock ", "")
	msg = strings.ReplaceAll(msg, "bedrock ", "")
	msg = strings.ReplaceAll(msg, " Bedrock", "")
	msg = strings.ReplaceAll(msg, " bedrock", "")
	return msg
}

// Downstream-facing error messages. Internal diagnostic strings (e.g.
// "quota exhausted on AmazonQ", "all endpoints failed", "No available
// accounts") must never reach the client — they leak upstream endpoint names
// and pool internals. These two replacements are the only error texts a
// downstream client is allowed to see for upstream/pool failures.
const (
	// msgServiceCoolingDown is shown when the upstream throttled/exhausted us
	// (HTTP 429 / "quota" errors). It nudges the client to back off briefly.
	msgServiceCoolingDown = "SERVICE IS CALLING DOWN. PLEASE TRY AGAIN AFTER 120 SECONDS."
	// msgServiceUnavailable is shown when no account could serve the request
	// or an unclassified upstream failure occurred.
	msgServiceUnavailable = "Service is temporarily unavailable, please try again!"
)

// classifyDownstreamError maps an internal upstream/pool error into a
// client-safe (httpStatus, message) pair, guaranteeing no internal diagnostic
// string leaks downstream. countAsFailure reports whether the caller should
// record a proxy-level failure (request-structure 400s are the client's fault,
// not ours, so they are not counted).
//
//   - nil (no account available)      -> 503, msgServiceUnavailable
//   - malformed request (HTTP 400)    -> 400, original detail (safe, client-side)
//   - quota/429 (upstream throttle)   -> 429, msgServiceCoolingDown
//   - anything else (5xx, net, etc.)  -> 503, msgServiceUnavailable
func classifyDownstreamError(err error) (status int, message string, countAsFailure bool) {
	if err == nil {
		return 503, msgServiceUnavailable, false
	}
	msg := err.Error()
	if isMalformedRequestErrorMessage(msg) {
		return 400, sanitizeUpstreamError(msg), false
	}
	if isQuotaErrorMessage(msg) {
		return 429, msgServiceCoolingDown, true
	}
	return 503, msgServiceUnavailable, true
}

// downstreamErrorMessage returns only the client-safe message for an error,
// for use mid-stream where the HTTP status has already been sent.
func downstreamErrorMessage(err error) string {
	_, msg, _ := classifyDownstreamError(err)
	return msg
}

func (h *Handler) disableAccount(account *config.Account, banStatus, banReason string) {
	if account == nil {
		return
	}

	updatedAccount := *account
	if !updatedAccount.Enabled && updatedAccount.BanStatus == banStatus && updatedAccount.BanReason == banReason {
		return
	}

	updatedAccount.Enabled = false
	updatedAccount.BanStatus = banStatus
	updatedAccount.BanReason = banReason
	updatedAccount.BanTime = time.Now().Unix()

	if err := config.UpdateAccount(account.ID, updatedAccount); err != nil {
		logger.Warnf("[AccountFailover] Failed to disable %s: %v", account.Email, err)
		return
	}

	logger.Warnf("[AccountFailover] Disabled %s: %s", account.Email, banReason)
	h.pool.Reload()
}

func (h *Handler) disableAccountOverage(account *config.Account) {
	if account == nil {
		return
	}

	snap, fetchErr := FetchOverageStatus(account)
	if fetchErr != nil {
		logger.Warnf("[AccountFailover] Failed to refresh overage status for %s: %v", account.Email, fetchErr)
		return
	}
	if persistErr := PersistOverageSnapshot(account.ID, snap); persistErr != nil {
		logger.Warnf("[AccountFailover] Failed to persist overage snapshot for %s: %v", account.Email, persistErr)
		return
	}

	logger.Warnf("[AccountFailover] Refreshed overage status for %s after upstream overage limit error: %s", account.Email, snap.Status)
	h.pool.Reload()
}

func (h *Handler) handleAccountFailure(account *config.Account, err error) {
	if account == nil || err == nil {
		return
	}

	errMsg := err.Error()
	switch {
	case isOverageErrorMessage(errMsg):
		h.disableAccountOverage(account)
		h.pool.RecordError(account.ID, false)
	case isQuotaErrorMessage(errMsg):
		h.pool.RecordError(account.ID, true)
	case isSuspensionErrorMessage(errMsg):
		h.disableAccount(account, "BANNED", "AWS temporarily suspended - unusual user activity detected")
	case isProfileUnavailableErrorMessage(errMsg):
		// Profile ARN may be transiently unresolvable (upstream blip, stale token).
		// Treat as a soft failure: short cooldown so the next request rotates account,
		// but never auto-disable — operators can still investigate via warn logs.
		h.pool.RecordError(account.ID, false)
	case isAuthErrorMessage(errMsg):
		h.disableAccount(account, "BANNED", "Authentication failed - token invalid or expired")
	default:
		h.pool.RecordError(account.ID, false)
	}
}
