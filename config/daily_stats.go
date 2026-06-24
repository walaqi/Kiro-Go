package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"kiro-go/logger"
)

// DailyStats holds aggregated usage for a single calendar day.
type DailyStats struct {
	Date           string             `json:"date"`
	TotalCredits   float64            `json:"totalCredits"`
	TotalRequests  int                `json:"totalRequests"`
	TotalTokens    int                `json:"totalTokens"`
	AccountCredits map[string]float64 `json:"accountCredits,omitempty"`
	ApiKeyCredits  map[string]float64 `json:"apiKeyCredits,omitempty"`
}

var (
	dailyMu    sync.Mutex
	dailyStats *DailyStats
)

func todayDateString() string {
	return time.Now().In(GetTimezone()).Format("2006-01-02")
}

func dailyStatsPath(date string) string {
	return filepath.Join(GetDataDir(), fmt.Sprintf("state-daily-%s.json", date))
}

func ensureDailyLoaded() {
	today := todayDateString()
	if dailyStats != nil && dailyStats.Date == today {
		return
	}
	dailyStats = loadDailyFile(today)
}

func loadDailyFile(date string) *DailyStats {
	path := dailyStatsPath(date)
	data, err := os.ReadFile(path)
	if err != nil {
		return &DailyStats{
			Date:           date,
			AccountCredits: make(map[string]float64),
			ApiKeyCredits:  make(map[string]float64),
		}
	}
	var ds DailyStats
	if err := json.Unmarshal(data, &ds); err != nil {
		logger.Warnf("[DailyStats] failed to parse %s: %v", path, err)
		return &DailyStats{
			Date:           date,
			AccountCredits: make(map[string]float64),
			ApiKeyCredits:  make(map[string]float64),
		}
	}
	if ds.AccountCredits == nil {
		ds.AccountCredits = make(map[string]float64)
	}
	if ds.ApiKeyCredits == nil {
		ds.ApiKeyCredits = make(map[string]float64)
	}
	return &ds
}

func persistDaily() {
	if dailyStats == nil {
		return
	}
	data, err := json.MarshalIndent(dailyStats, "", "  ")
	if err != nil {
		logger.Warnf("[DailyStats] marshal error: %v", err)
		return
	}
	path := dailyStatsPath(dailyStats.Date)
	if err := writeFileAtomic(path, data, 0600); err != nil {
		logger.Warnf("[DailyStats] write error: %v", err)
	}
}

// RecordDailyCredits adds a successful request's stats to the current day's aggregate.
func RecordDailyCredits(accountID, apiKeyID string, tokens int, credits float64) {
	dailyMu.Lock()
	defer dailyMu.Unlock()

	ensureDailyLoaded()

	dailyStats.TotalCredits += credits
	dailyStats.TotalRequests++
	dailyStats.TotalTokens += tokens

	if accountID != "" {
		dailyStats.AccountCredits[accountID] += credits
	}
	if apiKeyID != "" {
		dailyStats.ApiKeyCredits[apiKeyID] += credits
	}

	persistDaily()
}

// GetDailyStats returns the stats for the given date (format "2006-01-02").
// Returns nil if no data file exists for that date.
func GetDailyStats(date string) *DailyStats {
	dailyMu.Lock()
	defer dailyMu.Unlock()

	if dailyStats != nil && dailyStats.Date == date {
		cp := *dailyStats
		return &cp
	}

	path := dailyStatsPath(date)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	ds := loadDailyFile(date)
	return ds
}
