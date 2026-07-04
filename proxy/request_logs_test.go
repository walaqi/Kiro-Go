package proxy

import (
	"sync/atomic"
	"testing"
)

func TestAppendRequestLogSeparatesSuccessAndError(t *testing.T) {
	h := &Handler{}

	// Add success entries
	for i := 0; i < 10; i++ {
		h.appendRequestLog(RequestLog{Time: int64(i), Status: "success"})
	}
	// Add error entries
	for i := 0; i < 5; i++ {
		h.appendRequestLog(RequestLog{Time: int64(100 + i), Status: "error", Error: "test"})
	}

	h.requestLogsMu.RLock()
	successCount := len(h.requestLogs)
	errorCount := len(h.errorLogs)
	h.requestLogsMu.RUnlock()

	if successCount != 10 {
		t.Errorf("expected 10 success logs, got %d", successCount)
	}
	if errorCount != 5 {
		t.Errorf("expected 5 error logs, got %d", errorCount)
	}
}

func TestErrorLogsNotEvictedBySuccess(t *testing.T) {
	h := &Handler{}

	// Fill error logs first
	for i := 0; i < 3; i++ {
		h.appendRequestLog(RequestLog{Time: int64(i), Status: "error", Error: "err"})
	}

	// Flood with success entries beyond max
	for i := 0; i < requestLogsMaxSize+100; i++ {
		h.appendRequestLog(RequestLog{Time: int64(1000 + i), Status: "success"})
	}

	h.requestLogsMu.RLock()
	errorCount := len(h.errorLogs)
	successCount := len(h.requestLogs)
	h.requestLogsMu.RUnlock()

	// Error logs must remain untouched
	if errorCount != 3 {
		t.Errorf("error logs should not be evicted by success traffic: expected 3, got %d", errorCount)
	}
	// Success logs should be capped
	if successCount != requestLogsMaxSize {
		t.Errorf("success logs should be capped at %d, got %d", requestLogsMaxSize, successCount)
	}
}

func TestErrorLogsRingBufferCapsAtMax(t *testing.T) {
	h := &Handler{}

	for i := 0; i < errorLogsMaxSize+50; i++ {
		h.appendRequestLog(RequestLog{Time: int64(i), Status: "error", Error: "err"})
	}

	h.requestLogsMu.RLock()
	errorCount := len(h.errorLogs)
	h.requestLogsMu.RUnlock()

	if errorCount != errorLogsMaxSize {
		t.Errorf("error logs should be capped at %d, got %d", errorLogsMaxSize, errorCount)
	}
}

func TestGetRequestLogsMergesAndSortsByTime(t *testing.T) {
	h := &Handler{}

	h.appendRequestLog(RequestLog{Time: 10, Status: "success"})
	h.appendRequestLog(RequestLog{Time: 30, Status: "error", Error: "err"})
	h.appendRequestLog(RequestLog{Time: 20, Status: "success"})
	h.appendRequestLog(RequestLog{Time: 5, Status: "error", Error: "err"})

	logs := h.getRequestLogs()

	if len(logs) != 4 {
		t.Fatalf("expected 4 merged logs, got %d", len(logs))
	}

	// Should be newest first
	for i := 1; i < len(logs); i++ {
		if logs[i].Time > logs[i-1].Time {
			t.Errorf("logs not sorted newest first: [%d].Time=%d > [%d].Time=%d",
				i, logs[i].Time, i-1, logs[i-1].Time)
		}
	}
}

func TestApiClearLogsClearsBoth(t *testing.T) {
	h := &Handler{}

	h.appendRequestLog(RequestLog{Time: 1, Status: "success"})
	h.appendRequestLog(RequestLog{Time: 2, Status: "error", Error: "err"})

	// Simulate clear
	h.requestLogsMu.Lock()
	h.requestLogs = h.requestLogs[:0]
	h.errorLogs = h.errorLogs[:0]
	h.requestLogsMu.Unlock()

	logs := h.getRequestLogs()
	if len(logs) != 0 {
		t.Errorf("expected empty logs after clear, got %d", len(logs))
	}
}

func TestModerationLogsSeparateRingAndCarryInput(t *testing.T) {
	h := &Handler{}

	h.appendRequestLog(RequestLog{Time: 1, Status: "success"})
	h.appendRequestLog(RequestLog{Time: 2, Status: "error", Error: "err"})
	h.appendRequestLog(RequestLog{Time: 3, Status: "moderation", Model: "claude-opus-4", Input: "help me hack", MatchedRules: []int{1, 3}})

	h.requestLogsMu.RLock()
	successCount := len(h.requestLogs)
	errorCount := len(h.errorLogs)
	modCount := len(h.moderationLogs)
	h.requestLogsMu.RUnlock()

	if successCount != 1 || errorCount != 1 || modCount != 1 {
		t.Fatalf("expected 1/1/1 success/error/moderation, got %d/%d/%d", successCount, errorCount, modCount)
	}

	// Merged view includes the moderation entry with its Input preserved.
	logs := h.getRequestLogs()
	if len(logs) != 3 {
		t.Fatalf("expected 3 merged logs, got %d", len(logs))
	}
	var found bool
	for _, l := range logs {
		if l.Status == "moderation" {
			found = true
			if l.Input != "help me hack" {
				t.Errorf("moderation entry lost Input: %q", l.Input)
			}
			if l.Model != "claude-opus-4" {
				t.Errorf("moderation entry lost Model: %q", l.Model)
			}
			if len(l.MatchedRules) != 2 || l.MatchedRules[0] != 1 || l.MatchedRules[1] != 3 {
				t.Errorf("moderation entry lost MatchedRules: %v", l.MatchedRules)
			}
		}
	}
	if !found {
		t.Error("moderation entry not present in merged logs")
	}
}

func TestModerationLogsNotEvictedByOtherTraffic(t *testing.T) {
	h := &Handler{}

	h.appendRequestLog(RequestLog{Time: 1, Status: "moderation", Input: "keep me"})

	// Flood success + error beyond their caps.
	for i := 0; i < requestLogsMaxSize+50; i++ {
		h.appendRequestLog(RequestLog{Time: int64(1000 + i), Status: "success"})
	}
	for i := 0; i < errorLogsMaxSize+50; i++ {
		h.appendRequestLog(RequestLog{Time: int64(9000 + i), Status: "error", Error: "e"})
	}

	h.requestLogsMu.RLock()
	modCount := len(h.moderationLogs)
	h.requestLogsMu.RUnlock()

	if modCount != 1 {
		t.Errorf("moderation logs must not be evicted by other traffic: expected 1, got %d", modCount)
	}
}

func TestModerationLogsRingBufferCapsAtMax(t *testing.T) {
	h := &Handler{}

	for i := 0; i < moderationLogsMaxSize+50; i++ {
		h.appendRequestLog(RequestLog{Time: int64(i), Status: "moderation", Input: "x"})
	}

	h.requestLogsMu.RLock()
	modCount := len(h.moderationLogs)
	h.requestLogsMu.RUnlock()

	if modCount != moderationLogsMaxSize {
		t.Errorf("moderation logs should be capped at %d, got %d", moderationLogsMaxSize, modCount)
	}
}

// TestRecordFailureCountsIndependentlyOfLogging locks in the split-concern
// contract between recordFailure (counters) and recordFailureWithDetails (log
// ring). Regression guard: a prior change replaced a recordFailure() call with
// recordFailureWithDetails() at the mid-stream-disconnect paths, which froze the
// dashboard "failed" counter because WithDetails does not increment counters.
func TestRecordFailureCountsIndependentlyOfLogging(t *testing.T) {
	h := &Handler{}

	// recordFailureWithDetails logs but must NOT touch the counters.
	h.recordFailureWithDetails("claude", "m", "acct", errStub("boom"))
	if got := atomic.LoadInt64(&h.failedRequests); got != 0 {
		t.Fatalf("recordFailureWithDetails must not increment failedRequests, got %d", got)
	}
	if got := atomic.LoadInt64(&h.totalRequests); got != 0 {
		t.Fatalf("recordFailureWithDetails must not increment totalRequests, got %d", got)
	}
	h.requestLogsMu.RLock()
	logged := len(h.errorLogs)
	h.requestLogsMu.RUnlock()
	if logged != 1 {
		t.Fatalf("recordFailureWithDetails should append one error log, got %d", logged)
	}

	// recordFailure increments counters but must NOT write a log.
	h.recordFailure()
	if got := atomic.LoadInt64(&h.failedRequests); got != 1 {
		t.Fatalf("recordFailure should increment failedRequests to 1, got %d", got)
	}
	if got := atomic.LoadInt64(&h.totalRequests); got != 1 {
		t.Fatalf("recordFailure should increment totalRequests to 1, got %d", got)
	}
	h.requestLogsMu.RLock()
	loggedAfter := len(h.errorLogs)
	h.requestLogsMu.RUnlock()
	if loggedAfter != 1 {
		t.Fatalf("recordFailure must not append a log, error log count changed to %d", loggedAfter)
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }
