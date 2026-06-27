package proxy

import (
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
