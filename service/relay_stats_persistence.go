package service

import (
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// StatsPersistence handles saving and loading window summaries for crash recovery.
type StatsPersistence interface {
	SaveWindowSummaries(summaries []WindowSummary) error
	LoadWindowSummaries(since time.Time, limit int) ([]WindowSummary, error)
	DeleteBefore(before time.Time) (int64, error)
}

// ---------------------------------------------------------------------------
// GORM model — lives in service to avoid circular dependency with model pkg
// ---------------------------------------------------------------------------

type statsWindowSummaryRow struct {
	ID        int64     `gorm:"primaryKey;autoIncrement"`
	CreatedAt time.Time `gorm:"index"`

	WindowStart time.Time `gorm:"index;not null"`
	WindowEnd   time.Time `gorm:"not null"`

	ModelName string `gorm:"type:varchar(128);index"`
	ChannelID int
	GroupName string `gorm:"column:group_name;type:varchar(64)"`

	TotalAttempts     int64
	SuccessAttempts   int64
	FailedAttempts    int64
	ExcludedAttempts  int64
	ErrorLevel0       int64
	ErrorLevel1       int64
	ErrorLevel2       int64
	ErrorLevel3       int64
	TotalDurationNs   int64
	TotalFirstTokenNs int64
	FirstTokenCount   int64

	TPS             float64
	AvgDurationMs   float64
	AvgFirstTokenMs float64

	TotalRequests   int64
	SuccessRequests int64
	FailedRequests  int64
	RetryRequests   int64
	RetryRecovered  int64
	RecoveryRate    float64

	TaskExecCount      int64
	TaskExecSuccess    int64
	TaskExecDurationNs int64
	AvgExecDurationMs  float64

	ChannelScore float64
}

func (statsWindowSummaryRow) TableName() string {
	return "stats_window_summaries"
}

// ---------------------------------------------------------------------------
// DB-backed persistence
// ---------------------------------------------------------------------------

type dbPersistence struct {
	db *gorm.DB
}

// NewDBPersistence creates a DB-backed StatsPersistence and auto-migrates the table.
func NewDBPersistence(db *gorm.DB) StatsPersistence {
	if err := db.AutoMigrate(&statsWindowSummaryRow{}); err != nil {
		common.SysError("stats: failed to migrate stats_window_summaries: " + err.Error())
	}
	return &dbPersistence{db: db}
}

func (d *dbPersistence) SaveWindowSummaries(summaries []WindowSummary) error {
	if len(summaries) == 0 {
		return nil
	}
	rows := make([]statsWindowSummaryRow, 0, len(summaries))
	for _, s := range summaries {
		rows = append(rows, toRow(s))
	}
	return d.db.CreateInBatches(rows, 100).Error
}

func (d *dbPersistence) LoadWindowSummaries(since time.Time, limit int) ([]WindowSummary, error) {
	var rows []statsWindowSummaryRow
	q := d.db.Where("window_start >= ?", since).Order("window_start ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	summaries := make([]WindowSummary, 0, len(rows))
	for _, r := range rows {
		summaries = append(summaries, fromRow(r))
	}
	return summaries, nil
}

func (d *dbPersistence) DeleteBefore(before time.Time) (int64, error) {
	result := d.db.Where("window_start < ?", before).Delete(&statsWindowSummaryRow{})
	return result.RowsAffected, result.Error
}

// ---------------------------------------------------------------------------
// Conversion helpers
// ---------------------------------------------------------------------------

func toRow(s WindowSummary) statsWindowSummaryRow {
	return statsWindowSummaryRow{
		WindowStart:       s.WindowStart,
		WindowEnd:         s.WindowEnd,
		ModelName:         s.ModelName,
		ChannelID:         s.ChannelID,
		GroupName:         s.Group,
		TotalAttempts:     s.TotalAttempts,
		SuccessAttempts:   s.SuccessAttempts,
		FailedAttempts:    s.FailedAttempts,
		ExcludedAttempts:  s.ExcludedAttempts,
		ErrorLevel0:       s.ErrorLevelDist[0],
		ErrorLevel1:       s.ErrorLevelDist[1],
		ErrorLevel2:       s.ErrorLevelDist[2],
		ErrorLevel3:       s.ErrorLevelDist[3],
		TotalDurationNs:   s.TotalDurationNs,
		TotalFirstTokenNs: s.TotalFirstTokenNs,
		FirstTokenCount:   s.FirstTokenCount,
		TPS:               s.TPS,
		AvgDurationMs:     s.AvgDurationMs,
		AvgFirstTokenMs:   s.AvgFirstTokenMs,
		TotalRequests:     s.TotalRequests,
		SuccessRequests:   s.SuccessRequests,
		FailedRequests:    s.FailedRequests,
		RetryRequests:     s.RetryRequests,
		RetryRecovered:    s.RetryRecovered,
		RecoveryRate:      s.RecoveryRate,
		TaskExecCount:     s.TaskExecCount,
		TaskExecSuccess:   s.TaskExecSuccess,
		TaskExecDurationNs: s.TaskExecDurationNs,
		AvgExecDurationMs: s.AvgExecDurationMs,
		ChannelScore:      s.ChannelScore,
	}
}

func fromRow(r statsWindowSummaryRow) WindowSummary {
	return WindowSummary{
		WindowStart:       r.WindowStart,
		WindowEnd:         r.WindowEnd,
		ModelName:         r.ModelName,
		ChannelID:         r.ChannelID,
		Group:             r.GroupName,
		TotalAttempts:     r.TotalAttempts,
		SuccessAttempts:   r.SuccessAttempts,
		FailedAttempts:    r.FailedAttempts,
		ExcludedAttempts:  r.ExcludedAttempts,
		ErrorLevelDist:    [4]int64{r.ErrorLevel0, r.ErrorLevel1, r.ErrorLevel2, r.ErrorLevel3},
		TotalDurationNs:   r.TotalDurationNs,
		TotalFirstTokenNs: r.TotalFirstTokenNs,
		FirstTokenCount:   r.FirstTokenCount,
		TPS:               r.TPS,
		AvgDurationMs:     r.AvgDurationMs,
		AvgFirstTokenMs:   r.AvgFirstTokenMs,
		TotalRequests:     r.TotalRequests,
		SuccessRequests:   r.SuccessRequests,
		FailedRequests:    r.FailedRequests,
		RetryRequests:     r.RetryRequests,
		RetryRecovered:    r.RetryRecovered,
		RecoveryRate:      r.RecoveryRate,
		TaskExecCount:     r.TaskExecCount,
		TaskExecSuccess:   r.TaskExecSuccess,
		TaskExecDurationNs: r.TaskExecDurationNs,
		AvgExecDurationMs: r.AvgExecDurationMs,
		ChannelScore:      r.ChannelScore,
	}
}

// ---------------------------------------------------------------------------
// Cleanup goroutine
// ---------------------------------------------------------------------------

// StartStatsCleanup periodically deletes old window summary rows.
// Returns a stop function that can be called to halt the cleanup goroutine.
func StartStatsCleanup(p StatsPersistence, retentionDays int, interval time.Duration) (stop func()) {
	if retentionDays <= 0 {
		retentionDays = 7
	}
	if interval <= 0 {
		interval = 1 * time.Hour
	}
	stopCh := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				cutoff := time.Now().AddDate(0, 0, -retentionDays)
				deleted, err := p.DeleteBefore(cutoff)
				if err != nil {
					common.SysError("stats cleanup error: " + err.Error())
				} else if deleted > 0 {
					common.SysLog(fmt.Sprintf("stats cleanup: deleted %d rows before %s", deleted, cutoff.Format("2006-01-02")))
				}
			case <-stopCh:
				return
			}
		}
	}()
	return func() { close(stopCh) }
}
