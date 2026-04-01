package service

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// Collector interface
// ---------------------------------------------------------------------------

// RelayStatsCollector collects relay request events and provides query access.
// Implementations must be safe for concurrent use.
// CollectAttempt takes a pointer so classification results (Excluded, ErrorLevel)
// propagate back to the caller.
type RelayStatsCollector interface {
	CollectAttempt(event *AttemptEvent)
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
	// FirstTokenDuration is the time from attempt start to the first streaming chunk.
	// Non-streaming requests leave this at zero.
	FirstTokenDuration time.Duration `json:"first_token_duration_ns,omitempty"`
	CompletionTokens   int           `json:"completion_tokens,omitempty"`
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
	AvgOutputTPS     float64 `json:"avg_output_tps,omitempty"`
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

// ModelStats holds per-model statistics exposed to end users.
// Pointer fields (AvgDurationMs, AvgFirstTokenMs, TPS) are nil when no real
// data exists, which marshals to JSON null — distinguishable from an actual
// zero value by the frontend. HasData reports whether real traffic data was
// collected; when false the other numeric fields carry optimistic defaults
// (e.g. SuccessRate = 100).
type ModelStats struct {
	ModelName       string   `json:"model_name"`
	SuccessRate     float64  `json:"success_rate"`
	AvgDurationMs   *float64 `json:"avg_duration_ms,omitempty"`
	AvgFirstTokenMs *float64 `json:"avg_first_token_ms,omitempty"`
	TPS             *float64 `json:"tps,omitempty"`
	TotalRequests   int64    `json:"total_requests"`
	SuccessRequests int64    `json:"success_requests"`
	FailedRequests  int64    `json:"failed_requests"`
	HasData         bool     `json:"has_data"`
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

var (
	dimExtractorMu             sync.RWMutex
	windowDimensionExtractors = map[string]WindowDimensionExtractor{
		"model": func(s WindowSummary) string { return s.ModelName },
		"channel": func(s WindowSummary) string {
			if s.ChannelID == 0 {
				return ""
			}
			return strconv.Itoa(s.ChannelID)
		},
		"group": func(s WindowSummary) string { return s.Group },
	}
)

func RegisterWindowDimension(name string, extractor WindowDimensionExtractor) {
	dimExtractorMu.Lock()
	windowDimensionExtractors[name] = extractor
	dimExtractorMu.Unlock()
}

func buildWindowKeyFunc(dimensions []string) func(WindowSummary) string {
	dimExtractorMu.RLock()
	extractors := make([]WindowDimensionExtractor, 0, len(dimensions))
	for _, d := range dimensions {
		if ext, ok := windowDimensionExtractors[d]; ok {
			extractors = append(extractors, ext)
		}
	}
	dimExtractorMu.RUnlock()
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

func safeCollect(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			common.SysError(fmt.Sprintf("stats: panic in collector: %v", r))
		}
	}()
	fn()
}

func SafeCollectAttempt(collector RelayStatsCollector, event *AttemptEvent) {
	if !operation_setting.IsRelayStatsEnabled() {
		return
	}
	safeCollect(func() { collector.CollectAttempt(event) })
}

func SafeCollectRequestComplete(collector RelayStatsCollector, event RequestCompleteEvent) {
	if !operation_setting.IsRelayStatsEnabled() {
		return
	}
	safeCollect(func() { collector.CollectRequestComplete(event) })
}

func SafeCollectTaskExecution(collector RelayStatsCollector, event TaskExecutionEvent) {
	if !operation_setting.IsRelayStatsEnabled() {
		return
	}
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
	// Always register callbacks so runtime toggle from disabled→enabled works.
	operation_setting.OnStatsExclusionRulesUpdate = StatsErrorExclusionRulesFromJSON
	operation_setting.OnStatsScoreWeightsUpdate = UpdateScoreWeightsFromJSON

	if !operation_setting.IsRelayStatsEnabled() {
		// Keep noopCollector; no goroutines, no allocations.
		return
	}

	classifier := NewRuleBasedClassifier(nil)
	SetErrorClassifier(classifier)

	collector := NewMemoryStatsCollector(classifier, defaultWindowDuration, defaultRingBufferSize)
	SetRelayStatsCollector(collector)

	if json := operation_setting.StatsErrorExclusionRulesJSON; json != "" && json != "[]" {
		_ = StatsErrorExclusionRulesFromJSON(json)
	}
	if json := operation_setting.StatsScoreWeightsJSON; json != "" {
		_ = UpdateScoreWeightsFromJSON(json)
	}
}

// SetupStatsPersistence wires up DB persistence. Called after DB is ready.
// Skipped entirely when stats are disabled to avoid AutoMigrate, DB queries,
// and cleanup goroutines.
func SetupStatsPersistence(db *gorm.DB) {
	if !operation_setting.IsRelayStatsEnabled() {
		return
	}
	collector, ok := GetRelayStatsCollector().(*MemoryStatsCollector)
	if !ok {
		return
	}
	p := NewDBPersistence(db)
	collector.SetPersistence(p)
	collector.LoadFromDB(statsRetentionHours)

	// Seed channel scores from historical logs for cold/rare models that have
	// no recent window summary data. This runs after LoadFromDB so existing
	// (real) summaries are already present in the ring buffer, allowing
	// SeedFromLogs to skip combos that don't need seeding.
	existing := collector.GetWindowSummaries(0)
	seeds, err := SeedFromLogs(DefaultSeedLookbackDays, existing)
	if err != nil {
		common.SysError("stats: failed to seed from logs: " + err.Error())
	} else if len(seeds) > 0 {
		collector.InjectSeedSummaries(seeds)
	}

	_ = StartStatsCleanup(p, statsCleanupRetentionDays, 1*time.Hour)
}

// ---------------------------------------------------------------------------
// Noop implementations
// ---------------------------------------------------------------------------

type noopCollector struct{}

func (n *noopCollector) CollectAttempt(_ *AttemptEvent)                         {}
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
