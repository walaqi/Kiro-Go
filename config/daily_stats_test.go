package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func initDailyTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Reset global daily state between tests.
	dailyMu.Lock()
	dailyStats = nil
	dailyMu.Unlock()
	return dir
}

func TestRecordDailyCreditsAccumulates(t *testing.T) {
	dir := initDailyTest(t)

	RecordDailyCredits("acc1", "key1", 100, 1.5)
	RecordDailyCredits("acc1", "key1", 200, 2.5)
	RecordDailyCredits("acc2", "key2", 50, 0.5)

	today := todayDateString()
	ds := GetDailyStats(today)
	if ds == nil {
		t.Fatal("expected non-nil stats for today")
	}
	if ds.TotalRequests != 3 {
		t.Errorf("TotalRequests = %d, want 3", ds.TotalRequests)
	}
	if ds.TotalTokens != 350 {
		t.Errorf("TotalTokens = %d, want 350", ds.TotalTokens)
	}
	if ds.TotalCredits != 4.5 {
		t.Errorf("TotalCredits = %f, want 4.5", ds.TotalCredits)
	}
	if ds.AccountCredits["acc1"] != 4.0 {
		t.Errorf("AccountCredits[acc1] = %f, want 4.0", ds.AccountCredits["acc1"])
	}
	if ds.AccountCredits["acc2"] != 0.5 {
		t.Errorf("AccountCredits[acc2] = %f, want 0.5", ds.AccountCredits["acc2"])
	}
	if ds.ApiKeyCredits["key1"] != 4.0 {
		t.Errorf("ApiKeyCredits[key1] = %f, want 4.0", ds.ApiKeyCredits["key1"])
	}
	if ds.ApiKeyCredits["key2"] != 0.5 {
		t.Errorf("ApiKeyCredits[key2] = %f, want 0.5", ds.ApiKeyCredits["key2"])
	}

	// Verify file was persisted.
	path := filepath.Join(dir, "state-daily-"+today+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file not persisted: %v", err)
	}
	var ondisk DailyStats
	if err := json.Unmarshal(data, &ondisk); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ondisk.TotalRequests != 3 {
		t.Errorf("ondisk TotalRequests = %d, want 3", ondisk.TotalRequests)
	}
}

func TestRecordDailyModerationCredits(t *testing.T) {
	initDailyTest(t)

	// One normal request, then two moderation judge calls.
	RecordDailyCredits("acc1", "key1", 100, 2.0)
	RecordDailyModerationCredits("acc1", 0.3)
	RecordDailyModerationCredits("acc2", 0.5)

	today := todayDateString()
	ds := GetDailyStats(today)
	if ds == nil {
		t.Fatal("expected non-nil stats for today")
	}

	// Moderation credits are included in TotalCredits (2.0 + 0.3 + 0.5).
	if ds.TotalCredits != 2.8 {
		t.Errorf("TotalCredits = %f, want 2.8 (moderation included)", ds.TotalCredits)
	}
	// Isolated moderation subtotal.
	if ds.ModerationCredits != 0.8 {
		t.Errorf("ModerationCredits = %f, want 0.8", ds.ModerationCredits)
	}
	// Judge calls do NOT count as client requests/tokens.
	if ds.TotalRequests != 1 {
		t.Errorf("TotalRequests = %d, want 1 (judge calls excluded)", ds.TotalRequests)
	}
	if ds.TotalTokens != 100 {
		t.Errorf("TotalTokens = %d, want 100 (judge calls excluded)", ds.TotalTokens)
	}
	// Judge credits DO count against the account (it really spent them).
	if ds.AccountCredits["acc1"] != 2.3 {
		t.Errorf("AccountCredits[acc1] = %f, want 2.3", ds.AccountCredits["acc1"])
	}
	if ds.AccountCredits["acc2"] != 0.5 {
		t.Errorf("AccountCredits[acc2] = %f, want 0.5", ds.AccountCredits["acc2"])
	}
	// Judge credits are NOT attributed to any downstream API key.
	if ds.ApiKeyCredits["key1"] != 2.0 {
		t.Errorf("ApiKeyCredits[key1] = %f, want 2.0 (moderation excluded)", ds.ApiKeyCredits["key1"])
	}

	// Non-positive credits are a no-op.
	RecordDailyModerationCredits("acc1", 0)
	RecordDailyModerationCredits("acc1", -1)
	ds = GetDailyStats(today)
	if ds.ModerationCredits != 0.8 {
		t.Errorf("ModerationCredits changed by non-positive input: %f", ds.ModerationCredits)
	}
}

func TestRecordDailyCreditsEmptyIDs(t *testing.T) {
	initDailyTest(t)

	RecordDailyCredits("", "", 10, 0.1)

	today := todayDateString()
	ds := GetDailyStats(today)
	if ds == nil {
		t.Fatal("expected non-nil stats")
	}
	if ds.TotalRequests != 1 {
		t.Errorf("TotalRequests = %d, want 1", ds.TotalRequests)
	}
	if len(ds.AccountCredits) != 0 {
		t.Errorf("expected empty AccountCredits, got %v", ds.AccountCredits)
	}
	if len(ds.ApiKeyCredits) != 0 {
		t.Errorf("expected empty ApiKeyCredits, got %v", ds.ApiKeyCredits)
	}
}

func TestGetDailyStatsNonExistentDate(t *testing.T) {
	initDailyTest(t)

	ds := GetDailyStats("1999-01-01")
	if ds != nil {
		t.Errorf("expected nil for non-existent date, got %+v", ds)
	}
}

func TestGetDailyStatsLoadsFromFile(t *testing.T) {
	dir := initDailyTest(t)

	date := "2025-03-15"
	saved := DailyStats{
		Date:           date,
		TotalCredits:   12.5,
		TotalRequests:  7,
		TotalTokens:    4200,
		AccountCredits: map[string]float64{"a1": 10.0, "a2": 2.5},
		ApiKeyCredits:  map[string]float64{"k1": 12.5},
	}
	data, _ := json.Marshal(saved)
	path := filepath.Join(dir, "state-daily-"+date+".json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	ds := GetDailyStats(date)
	if ds == nil {
		t.Fatal("expected non-nil stats from file")
	}
	if ds.TotalCredits != 12.5 {
		t.Errorf("TotalCredits = %f, want 12.5", ds.TotalCredits)
	}
	if ds.TotalRequests != 7 {
		t.Errorf("TotalRequests = %d, want 7", ds.TotalRequests)
	}
	if ds.AccountCredits["a1"] != 10.0 {
		t.Errorf("AccountCredits[a1] = %f, want 10.0", ds.AccountCredits["a1"])
	}
}

func TestRecordDailyStatsConcurrent(t *testing.T) {
	initDailyTest(t)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			RecordDailyCredits("acc", "key", 10, 1.0)
		}()
	}
	wg.Wait()

	today := todayDateString()
	ds := GetDailyStats(today)
	if ds == nil {
		t.Fatal("expected non-nil stats")
	}
	if ds.TotalRequests != 50 {
		t.Errorf("TotalRequests = %d, want 50", ds.TotalRequests)
	}
	if ds.TotalTokens != 500 {
		t.Errorf("TotalTokens = %d, want 500", ds.TotalTokens)
	}
	if ds.TotalCredits != 50.0 {
		t.Errorf("TotalCredits = %f, want 50.0", ds.TotalCredits)
	}
}

func TestGetDailyStatsReturnsCopy(t *testing.T) {
	initDailyTest(t)

	RecordDailyCredits("acc", "key", 10, 1.0)
	today := todayDateString()

	ds1 := GetDailyStats(today)
	ds1.TotalCredits = 999.0

	ds2 := GetDailyStats(today)
	if ds2.TotalCredits == 999.0 {
		t.Error("GetDailyStats should return a copy, not a reference to internal state")
	}
}

func TestGetTimezoneDefault(t *testing.T) {
	initDailyTest(t)

	loc := GetTimezone()
	if loc == nil {
		t.Fatal("GetTimezone returned nil")
	}
	if loc.String() != "Asia/Shanghai" {
		t.Errorf("default timezone = %q, want Asia/Shanghai", loc.String())
	}
}

func TestDailyStatsCorruptFile(t *testing.T) {
	dir := initDailyTest(t)

	date := "2025-06-01"
	path := filepath.Join(dir, "state-daily-"+date+".json")
	if err := os.WriteFile(path, []byte("not json {{{"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	ds := GetDailyStats(date)
	if ds == nil {
		t.Fatal("expected non-nil fallback for corrupt file")
	}
	if ds.TotalRequests != 0 {
		t.Errorf("expected zero stats from corrupt file, got %d", ds.TotalRequests)
	}
}
