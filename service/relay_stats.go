package service

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// Collector interface
// ---------------------------------------------------------------------------

// RelayStatsCollector collects relay request events and provides query access.
// Implementations must be safe for concurrent use.
type RelayStatsCollector interface {
	CollectAttempt(event AttemptEvent)
	CollectRequestComplete(event RequestCompleteEvent)
	CollectTaskExecution(event TaskExecutionEvent)
	GetCounters() StatsCounters
	GetWindowSummaries(limit int) []WindowSummary
	GetTimeSeries(query TimeSeriesQuery) TimeSeriesResult
	AggregateWindows(dimensions []string) map[string]StatsCounters
	GetModelStats(startTime, endTime int64) []ModelStats
	Reset()
}

// ErrorClassifier determines whether a failed attempt should be excluded,
// and assigns an error severity level (0=excluded, 1=normal, 2=serious, 3=critical).
type ErrorClassifier interface {
	Classify(event AttemptEvent) (excluded bool, level int, reason string)
	ClassifyTaskFailReason(modelName string, failReason string) (excluded bool, level int, reason string)
}

// ---------------------------------------------------------------------------
// Event structs
// ---------------------------------------------------------------------------

// AttemptEvent represents a single relay attempt (one try within a retry loop).
type AttemptEvent struct {
	Timestamp          time.Time     `json:"timestamp"`
	RequestID          string        `json:"request_id"`
	AttemptIndex       int           `json:"attempt_index"`
	ChannelID          int           `json:"channel_id"`
	ChannelType        int           `json:"channel_type"`
	ChannelName        string        `json:"channel_name"`
	ModelName          string        `json:"model_name"`
	Group              string        `json:"group"`
	IsAsync            bool          `json:"is_async"`
	Success            bool          `json:"success"`
	StatusCode         int           `json:"status_code,omitempty"`
	ErrorCode          string        `json:"error_code,omitempty"`
	ErrorType          string        `json:"error_type,omitempty"`
	ErrorMessage       string        `json:"error_message,omitempty"`
	ErrorLevel         int           `json:"error_level,omitempty"`
	Duration           time.Duration `json:"duration_ns"`
	FirstTokenDuration time.Duration `json:"first_token_duration_ns,omitempty"`
	Excluded           bool          `json:"excluded"`
	ExcludeReason      string        `json:"exclude_reason,omitempty"`
}

// RequestCompleteEvent represents the final outcome of a relay request.
type RequestCompleteEvent struct {
	Timestamp            time.Time     `json:"timestamp"`
	RequestID            string        `json:"request_id"`
	UserID               int           `json:"user_id"`
	TokenID              int           `json:"token_id"`
	Group                string        `json:"group"`
	OriginalModel        string        `json:"original_model"`
	RelayMode            int           `json:"relay_mode"`
	IsAsync              bool          `json:"is_async"`
	TotalAttempts        int           `json:"total_attempts"`
	FinalSuccess         bool          `json:"final_success"`
	HasRetry             bool          `json:"has_retry"`
	RetryRecovered       bool          `json:"retry_recovered"`
	ChannelChain         []int         `json:"channel_chain"`
	TotalDuration        time.Duration `json:"total_duration_ns"`
	FirstErrorCode       string        `json:"first_error_code,omitempty"`
	FirstErrorStatusCode int           `json:"first_error_status_code,omitempty"`
	ExcludedAttempts     int           `json:"excluded_attempts"`
	RealErrorAttempts    int           `json:"real_error_attempts"`
}

// TaskExecutionEvent represents the completion of an async task's execution phase
// (polled from upstream until reaching SUCCESS or FAILURE terminal state).
type TaskExecutionEvent struct {
	Timestamp         time.Time             `json:"timestamp"`
	TaskID            string                `json:"task_id"`
	Platform          constant.TaskPlatform `json:"platform"`
	ModelName         string                `json:"model_name"`
	ChannelID         int                   `json:"channel_id"`
	Group             string                `json:"group"`
	Success           bool                  `json:"success"`
	FailReason        string                `json:"fail_reason,omitempty"`
	SubmitTime        int64                 `json:"submit_time"`
	FinishTime        int64                 `json:"finish_time"`
	ExecutionDuration time.Duration         `json:"execution_duration_ns"`
	Excluded          bool                  `json:"excluded"`
	ExcludeReason     string                `json:"exclude_reason,omitempty"`
	ErrorLevel        int                   `json:"error_level,omitempty"`
}

// ---------------------------------------------------------------------------
// Aggregated counters (lifetime totals)
// ---------------------------------------------------------------------------

// StatsCounters holds aggregated counters for JSON serialization.
type StatsCounters struct {
	// Request-level (final outcome)
	TotalRequests   int64   `json:"total_requests"`
	SuccessRequests int64   `json:"success_requests"`
	FailedRequests  int64   `json:"failed_requests"`
	RetryRequests   int64   `json:"retry_requests"`
	RetryRecovered  int64   `json:"retry_recovered"`
	RecoveryRate    float64 `json:"retry_recovery_rate"`

	// Attempt-level (per-try)
	TotalAttempts    int64   `json:"total_attempts"`
	SuccessAttempts  int64   `json:"success_attempts"`
	FailedAttempts   int64   `json:"failed_attempts"`
	ExcludedAttempts int64   `json:"excluded_attempts"`
	TPS              float64 `json:"tps,omitempty"`
	AvgDurationMs    float64 `json:"avg_duration_ms,omitempty"`
	AvgFirstTokenMs  float64 `json:"avg_first_token_ms,omitempty"`

	// Async task
	TaskSubmitCount   int64   `json:"task_submit_count"`
	TaskSubmitSuccess int64   `json:"task_submit_success"`
	TaskExecCount     int64   `json:"task_exec_count"`
	TaskExecSuccess   int64   `json:"task_exec_success"`
	AvgExecDurationMs float64 `json:"avg_exec_duration_ms,omitempty"`
}

// ---------------------------------------------------------------------------
// ModelStats — user-facing, sanitized per-model statistics
// ---------------------------------------------------------------------------

type ModelStats struct {
	ModelName       string  `json:"model_name"`
	SuccessRate     float64 `json:"success_rate"`
	AvgDurationMs   float64 `json:"avg_duration_ms"`
	AvgFirstTokenMs float64 `json:"avg_first_token_ms"`
	TotalRequests   int64   `json:"total_requests"`
	SuccessRequests int64   `json:"success_requests"`
	FailedRequests  int64   `json:"failed_requests"`
}

// ---------------------------------------------------------------------------
// Time series query / result (for chart API)
// ---------------------------------------------------------------------------

type TimeSeriesQuery struct {
	GroupBy  string        // dimension: model, channel, group
	Metric  string        // success_rate, tps, avg_duration, avg_first_token, channel_score, task_exec_success_rate
	Interval time.Duration // aggregation interval, 0 = raw window size
	Range   time.Duration // how far back to look
}

type TimeSeriesPoint struct {
	Time  time.Time `json:"time"`
	Value float64   `json:"value"`
}

type TimeSeries struct {
	Key    string            `json:"key"`
	Label  string            `json:"label"`
	Points []TimeSeriesPoint `json:"points"`
}

type TimeSeriesResult struct {
	Series  []TimeSeries `json:"series"`
	Metric  string       `json:"metric"`
	Interval string      `json:"interval"`
	GroupBy  string       `json:"group_by"`
}

// ---------------------------------------------------------------------------
// Dimension extractors
// ---------------------------------------------------------------------------

type WindowDimensionExtractor func(s WindowSummary) string

var windowDimensionExtractors = map[string]WindowDimensionExtractor{
	"model": func(s WindowSummary) string { return s.ModelName },
	"channel": func(s WindowSummary) string {
		if s.ChannelID == 0 {
			return ""
		}
		return strconv.Itoa(s.ChannelID)
	},
	"group": func(s WindowSummary) string { return s.Group },
}

func RegisterWindowDimension(name string, extractor WindowDimensionExtractor) {
	windowDimensionExtractors[name] = extractor
}

func buildWindowKeyFunc(dimensions []string) func(WindowSummary) string {
	extractors := make([]WindowDimensionExtractor, 0, len(dimensions))
	for _, d := range dimensions {
		if ext, ok := windowDimensionExtractors[d]; ok {
			extractors = append(extractors, ext)
		}
	}
	if len(extractors) == 0 {
		return nil
	}
	if len(extractors) == 1 {
		return func(s WindowSummary) string { return extractors[0](s) }
	}
	return func(s WindowSummary) string {
		parts := make([]string, len(extractors))
		for i, ext := range extractors {
			parts[i] = ext(s)
		}
		return strings.Join(parts, ":")
	}
}

// ---------------------------------------------------------------------------
// Global registry
// ---------------------------------------------------------------------------

var (
	collectorMu       sync.RWMutex
	defaultCollector  RelayStatsCollector = &noopCollector{}
	classifierMu      sync.RWMutex
	defaultClassifier ErrorClassifier = &noopClassifier{}
)

func SetRelayStatsCollector(c RelayStatsCollector)   { collectorMu.Lock(); defaultCollector = c; collectorMu.Unlock() }
func GetRelayStatsCollector() RelayStatsCollector     { collectorMu.RLock(); defer collectorMu.RUnlock(); return defaultCollector }
func SetErrorClassifier(c ErrorClassifier)            { classifierMu.Lock(); defaultClassifier = c; classifierMu.Unlock() }
func GetErrorClassifier() ErrorClassifier             { classifierMu.RLock(); defer classifierMu.RUnlock(); return defaultClassifier }

// ---------------------------------------------------------------------------
// Safe wrappers — panics never affect business flow
// ---------------------------------------------------------------------------

func safeCollect(fn func()) { defer func() { recover() }(); fn() }

func SafeCollectAttempt(collector RelayStatsCollector, event AttemptEvent) {
	safeCollect(func() { collector.CollectAttempt(event) })
}

func SafeCollectRequestComplete(collector RelayStatsCollector, event RequestCompleteEvent) {
	safeCollect(func() { collector.CollectRequestComplete(event) })
}

func SafeCollectTaskExecution(collector RelayStatsCollector, event TaskExecutionEvent) {
	safeCollect(func() { collector.CollectTaskExecution(event) })
}

// ---------------------------------------------------------------------------
// Initialization
// ---------------------------------------------------------------------------

// statsRetentionHours controls how far back summaries are loaded on startup.
const statsRetentionHours = 24 * 7 // 7 days

// statsCleanupRetentionDays controls how old summaries must be before deletion.
const statsCleanupRetentionDays = 7

func InitRelayStats() {
	classifier := NewRuleBasedClassifier(nil)
	SetErrorClassifier(classifier)

	collector := NewMemoryStatsCollector(classifier, defaultWindowDuration, defaultRingBufferSize)
	SetRelayStatsCollector(collector)

	operation_setting.OnStatsExclusionRulesUpdate = StatsErrorExclusionRulesFromJSON

	if json := operation_setting.StatsErrorExclusionRulesJSON; json != "" && json != "[]" {
		_ = StatsErrorExclusionRulesFromJSON(json)
	}
}

// SetupStatsPersistence wires up DB persistence. Called after DB is ready.
func SetupStatsPersistence(db *gorm.DB) {
	collector, ok := GetRelayStatsCollector().(*MemoryStatsCollector)
	if !ok {
		return
	}
	p := NewDBPersistence(db)
	collector.SetPersistence(p)
	collector.LoadFromDB(statsRetentionHours)
	StartStatsCleanup(p, statsCleanupRetentionDays, 1*time.Hour)
}

// ---------------------------------------------------------------------------
// Noop implementations
// ---------------------------------------------------------------------------

type noopCollector struct{}

func (n *noopCollector) CollectAttempt(_ AttemptEvent)                          {}
func (n *noopCollector) CollectRequestComplete(_ RequestCompleteEvent)          {}
func (n *noopCollector) CollectTaskExecution(_ TaskExecutionEvent)              {}
func (n *noopCollector) GetCounters() StatsCounters                            { return StatsCounters{} }
func (n *noopCollector) GetWindowSummaries(_ int) []WindowSummary              { return nil }
func (n *noopCollector) GetTimeSeries(_ TimeSeriesQuery) TimeSeriesResult      { return TimeSeriesResult{} }
func (n *noopCollector) AggregateWindows(_ []string) map[string]StatsCounters  { return nil }
func (n *noopCollector) GetModelStats(_, _ int64) []ModelStats                 { return nil }
func (n *noopCollector) Reset()                                                {}

type noopClassifier struct{}

func (n *noopClassifier) Classify(_ AttemptEvent) (bool, int, string)                      { return false, 1, "" }
func (n *noopClassifier) ClassifyTaskFailReason(_ string, _ string) (bool, int, string)    { return false, 1, "" }
