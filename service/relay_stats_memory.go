package service

import (
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
)

const defaultRingBufferSize = 10000

// ---------------------------------------------------------------------------
// RingBuffer — generic fixed-capacity circular buffer
// ---------------------------------------------------------------------------

type RingBuffer[T any] struct {
	mu    sync.RWMutex
	buf   []T
	cap   int
	head  int
	count int
}

func NewRingBuffer[T any](capacity int) *RingBuffer[T] {
	if capacity <= 0 {
		capacity = defaultRingBufferSize
	}
	return &RingBuffer[T]{buf: make([]T, capacity), cap: capacity}
}

func (r *RingBuffer[T]) Push(item T) {
	r.mu.Lock()
	r.buf[r.head] = item
	r.head = (r.head + 1) % r.cap
	if r.count < r.cap {
		r.count++
	}
	r.mu.Unlock()
}

func (r *RingBuffer[T]) PushBatch(items []T) {
	r.mu.Lock()
	for _, item := range items {
		r.buf[r.head] = item
		r.head = (r.head + 1) % r.cap
		if r.count < r.cap {
			r.count++
		}
	}
	r.mu.Unlock()
}

func (r *RingBuffer[T]) Snapshot(limit int) []T {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := r.count
	if limit > 0 && limit < n {
		n = limit
	}
	if n == 0 {
		return nil
	}
	result := make([]T, n)
	start := (r.head - n + r.cap) % r.cap
	for i := 0; i < n; i++ {
		result[i] = r.buf[(start+i)%r.cap]
	}
	return result
}

func (r *RingBuffer[T]) Reset() {
	r.mu.Lock()
	r.head = 0
	r.count = 0
	r.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Atomic lifetime counters
// ---------------------------------------------------------------------------

type atomicCounters struct {
	totalRequests    atomic.Int64
	successRequests  atomic.Int64
	failedRequests   atomic.Int64
	totalAttempts    atomic.Int64
	successAttempts  atomic.Int64
	failedAttempts   atomic.Int64
	excludedAttempts atomic.Int64
	retryRequests    atomic.Int64
	retryRecovered   atomic.Int64
	taskSubmitCount  atomic.Int64
	taskSubmitSuccess atomic.Int64
	taskExecCount    atomic.Int64
	taskExecSuccess  atomic.Int64
}

func (a *atomicCounters) snapshot() StatsCounters {
	retry := a.retryRequests.Load()
	recovered := a.retryRecovered.Load()
	var rate float64
	if retry > 0 {
		rate = float64(recovered) / float64(retry)
	}
	return StatsCounters{
		TotalRequests:     a.totalRequests.Load(),
		SuccessRequests:   a.successRequests.Load(),
		FailedRequests:    a.failedRequests.Load(),
		TotalAttempts:     a.totalAttempts.Load(),
		SuccessAttempts:   a.successAttempts.Load(),
		FailedAttempts:    a.failedAttempts.Load(),
		ExcludedAttempts:  a.excludedAttempts.Load(),
		RetryRequests:     retry,
		RetryRecovered:    recovered,
		RecoveryRate:      rate,
		TaskSubmitCount:   a.taskSubmitCount.Load(),
		TaskSubmitSuccess: a.taskSubmitSuccess.Load(),
		TaskExecCount:     a.taskExecCount.Load(),
		TaskExecSuccess:   a.taskExecSuccess.Load(),
	}
}

func (a *atomicCounters) reset() {
	a.totalRequests.Store(0)
	a.successRequests.Store(0)
	a.failedRequests.Store(0)
	a.totalAttempts.Store(0)
	a.successAttempts.Store(0)
	a.failedAttempts.Store(0)
	a.excludedAttempts.Store(0)
	a.retryRequests.Store(0)
	a.retryRecovered.Store(0)
	a.taskSubmitCount.Store(0)
	a.taskSubmitSuccess.Store(0)
	a.taskExecCount.Store(0)
	a.taskExecSuccess.Store(0)
}

// ---------------------------------------------------------------------------
// MemoryStatsCollector
// ---------------------------------------------------------------------------

// MemoryStatsCollector is a RelayStatsCollector backed by in-memory RingBuffer
// with optional DB persistence.
// Raw events flow into a WindowBuffer; on flush, aggregated WindowSummary
// records are pushed into a RingBuffer and persisted to DB.
type MemoryStatsCollector struct {
	windowBuf   *WindowBuffer
	summaries   *RingBuffer[WindowSummary]
	counters    atomicCounters
	persistence StatsPersistence
}

func NewMemoryStatsCollector(classifier ErrorClassifier, windowDuration time.Duration, bufSize int) *MemoryStatsCollector {
	m := &MemoryStatsCollector{
		summaries: NewRingBuffer[WindowSummary](bufSize),
	}
	m.windowBuf = NewWindowBuffer(classifier, windowDuration, m.onWindowFlush)
	return m
}

func (m *MemoryStatsCollector) SetPersistence(p StatsPersistence) {
	m.persistence = p
}

// LoadFromDB restores window summaries from the database into the RingBuffer.
func (m *MemoryStatsCollector) LoadFromDB(retentionHours int) {
	if m.persistence == nil {
		return
	}
	since := time.Now().Add(-time.Duration(retentionHours) * time.Hour)
	summaries, err := m.persistence.LoadWindowSummaries(since, 0)
	if err != nil {
		common.SysError("stats: failed to load summaries from DB: " + err.Error())
		return
	}
	if len(summaries) > 0 {
		m.summaries.PushBatch(summaries)
		common.SysLog("stats: loaded " + strconv.Itoa(len(summaries)) + " window summaries from DB")
	}
}

// onWindowFlush is called by WindowBuffer after each time window.
func (m *MemoryStatsCollector) onWindowFlush(summaries []WindowSummary) {
	m.summaries.PushBatch(summaries)
	if m.persistence != nil {
		if err := m.persistence.SaveWindowSummaries(summaries); err != nil {
			common.SysError("stats: failed to persist window summaries: " + err.Error())
		}
	}
}

func (m *MemoryStatsCollector) CollectAttempt(event *AttemptEvent) {
	m.windowBuf.CollectAttempt(event)

	m.counters.totalAttempts.Add(1)
	if event.Success {
		m.counters.successAttempts.Add(1)
	} else if event.Excluded {
		m.counters.excludedAttempts.Add(1)
	} else {
		m.counters.failedAttempts.Add(1)
	}

	if event.IsAsync {
		m.counters.taskSubmitCount.Add(1)
		if event.Success {
			m.counters.taskSubmitSuccess.Add(1)
		}
	}
}

func (m *MemoryStatsCollector) CollectRequestComplete(event RequestCompleteEvent) {
	m.windowBuf.CollectRequestComplete(&event)

	m.counters.totalRequests.Add(1)
	if event.FinalSuccess {
		m.counters.successRequests.Add(1)
	} else {
		m.counters.failedRequests.Add(1)
	}
	if event.HasRetry {
		m.counters.retryRequests.Add(1)
	}
	if event.RetryRecovered {
		m.counters.retryRecovered.Add(1)
	}
}

func (m *MemoryStatsCollector) CollectTaskExecution(event TaskExecutionEvent) {
	m.windowBuf.CollectTaskExecution(&event)

	m.counters.taskExecCount.Add(1)
	if event.Success {
		m.counters.taskExecSuccess.Add(1)
	}
}

func (m *MemoryStatsCollector) GetCounters() StatsCounters {
	return m.counters.snapshot()
}

func (m *MemoryStatsCollector) GetWindowSummaries(limit int) []WindowSummary {
	result := m.summaries.Snapshot(limit)
	result = append(result, m.windowBuf.Peek()...)
	return result
}

func (m *MemoryStatsCollector) AggregateWindows(dimensions []string) map[string]StatsCounters {
	keyFunc := buildWindowKeyFunc(dimensions)
	if keyFunc == nil {
		return nil
	}
	windows := m.summaries.Snapshot(0)
	windows = append(windows, m.windowBuf.Peek()...)
	buckets := make(map[string]*windowAgg)
	for _, w := range windows {
		key := keyFunc(w)
		if key == "" {
			continue
		}
		agg, ok := buckets[key]
		if !ok {
			agg = &windowAgg{}
			buckets[key] = agg
		}
		agg.addWindow(w)
	}
	result := make(map[string]StatsCounters, len(buckets))
	for k, agg := range buckets {
		result[k] = agg.toCounters()
	}
	return result
}

// GetTimeSeries builds time series data for chart rendering.
func (m *MemoryStatsCollector) GetTimeSeries(query TimeSeriesQuery) TimeSeriesResult {
	windows := m.summaries.Snapshot(0)
	windows = append(windows, m.windowBuf.Peek()...)

	cutoff := time.Now().Add(-query.Range)
	var filtered []WindowSummary
	for _, w := range windows {
		if w.WindowEnd.After(cutoff) {
			filtered = append(filtered, w)
		}
	}

	// Group by dimension value
	keyFunc := buildWindowKeyFunc([]string{query.GroupBy})
	if keyFunc == nil {
		keyFunc = func(_ WindowSummary) string { return "all" }
	}

	type bucketEntry struct {
		windows []WindowSummary
	}
	dimBuckets := make(map[string]*bucketEntry)
	for _, w := range filtered {
		key := keyFunc(w)
		if key == "" {
			continue
		}
		be, ok := dimBuckets[key]
		if !ok {
			be = &bucketEntry{}
			dimBuckets[key] = be
		}
		be.windows = append(be.windows, w)
	}

	interval := query.Interval
	if interval <= 0 {
		interval = defaultWindowDuration
	}

	var series []TimeSeries
	for key, be := range dimBuckets {
		points := buildTimeSeriesPoints(be.windows, query.Metric, interval, cutoff)
		series = append(series, TimeSeries{
			Key:    key,
			Label:  key,
			Points: points,
		})
	}

	return TimeSeriesResult{
		Series:   series,
		Metric:   query.Metric,
		Interval: formatDuration(interval),
		GroupBy:  query.GroupBy,
	}
}

// GetModelStats returns user-facing per-model statistics aggregated from
// window summaries within the given unix timestamp range (0 = no filter).
func (m *MemoryStatsCollector) GetModelStats(startTime, endTime int64) []ModelStats {
	windows := m.summaries.Snapshot(0)
	// Include current unflushed window so data is real-time
	windows = append(windows, m.windowBuf.Peek()...)

	type modelAgg struct {
		totalAttempts     int64
		successAttempts   int64
		excludedAttempts  int64
		totalDurationNs   int64
		totalFirstTokenNs int64
		firstTokenCount   int64
		totalRequests     int64
		successRequests   int64
		failedRequests    int64
	}
	buckets := make(map[string]*modelAgg)

	for _, w := range windows {
		if startTime > 0 && w.WindowEnd.Unix() < startTime {
			continue
		}
		if endTime > 0 && w.WindowStart.Unix() > endTime {
			continue
		}
		if w.ModelName == "" {
			continue
		}
		agg, ok := buckets[w.ModelName]
		if !ok {
			agg = &modelAgg{}
			buckets[w.ModelName] = agg
		}
		agg.totalAttempts += w.TotalAttempts
		agg.successAttempts += w.SuccessAttempts
		agg.excludedAttempts += w.ExcludedAttempts
		agg.totalDurationNs += w.TotalDurationNs
		agg.totalFirstTokenNs += w.TotalFirstTokenNs
		agg.firstTokenCount += w.FirstTokenCount
		agg.totalRequests += w.TotalRequests
		agg.successRequests += w.SuccessRequests
		agg.failedRequests += w.FailedRequests
	}

	result := make([]ModelStats, 0, len(buckets))
	for model, agg := range buckets {
		// User-facing success rate = final request outcome, not per-attempt
		var successRate float64
		if agg.totalRequests > 0 {
			successRate = float64(agg.successRequests) / float64(agg.totalRequests) * 100
		} else {
			successRate = 100
		}
		var avgDur float64
		if agg.totalAttempts > 0 {
			avgDur = float64(agg.totalDurationNs) / float64(agg.totalAttempts) / 1e6
		}
		var avgFT float64
		if agg.firstTokenCount > 0 {
			avgFT = float64(agg.totalFirstTokenNs) / float64(agg.firstTokenCount) / 1e6
		}
		result = append(result, ModelStats{
			ModelName:       model,
			SuccessRate:     successRate,
			AvgDurationMs:   avgDur,
			AvgFirstTokenMs: avgFT,
			TotalRequests:   agg.totalRequests,
			SuccessRequests: agg.successRequests,
			FailedRequests:  agg.failedRequests,
		})
	}
	return result
}

func (m *MemoryStatsCollector) Reset() {
	m.counters.reset()
	m.summaries.Reset()
	m.windowBuf.Reset()
}

// ---------------------------------------------------------------------------
// Time series helpers
// ---------------------------------------------------------------------------

func buildTimeSeriesPoints(windows []WindowSummary, metric string, interval time.Duration, cutoff time.Time) []TimeSeriesPoint {
	if len(windows) == 0 {
		return nil
	}

	// Group windows into time slots
	type slot struct {
		agg   windowAgg
		count int
	}
	slots := make(map[int64]*slot) // slot key = truncated unix
	for _, w := range windows {
		slotTime := w.WindowStart.Truncate(interval).Unix()
		s, ok := slots[slotTime]
		if !ok {
			s = &slot{}
			slots[slotTime] = s
		}
		s.agg.addWindow(w)
		s.count++
	}

	points := make([]TimeSeriesPoint, 0, len(slots))
	for ts, s := range slots {
		t := time.Unix(ts, 0)
		if t.Before(cutoff) {
			continue
		}
		value := extractMetricValue(s.agg, metric)
		points = append(points, TimeSeriesPoint{Time: t, Value: value})
	}

	// Sort by time
	for i := 1; i < len(points); i++ {
		for j := i; j > 0 && points[j].Time.Before(points[j-1].Time); j-- {
			points[j], points[j-1] = points[j-1], points[j]
		}
	}
	return points
}

func extractMetricValue(agg windowAgg, metric string) float64 {
	switch metric {
	case "success_rate":
		effective := agg.totalAttempts - agg.excludedAttempts
		if effective <= 0 {
			return 100
		}
		return float64(agg.successAttempts) / float64(effective) * 100
	case "tps":
		if agg.windowSeconds > 0 {
			return float64(agg.totalAttempts) / agg.windowSeconds
		}
		return 0
	case "avg_duration":
		if agg.totalAttempts > 0 {
			return float64(agg.totalDurationNs) / float64(agg.totalAttempts) / 1e6
		}
		return 0
	case "avg_first_token":
		if agg.firstTokenCount > 0 {
			return float64(agg.totalFirstTokenNs) / float64(agg.firstTokenCount) / 1e6
		}
		return 0
	case "channel_score":
		return agg.channelScoreSum / float64(max64(int64(agg.windowCount), 1))
	case "request_success_rate":
		if agg.totalRequests > 0 {
			return float64(agg.successRequests) / float64(agg.totalRequests) * 100
		}
		return 100
	case "task_exec_success_rate":
		if agg.taskExecCount > 0 {
			return float64(agg.taskExecSuccess) / float64(agg.taskExecCount) * 100
		}
		return 100
	default:
		return 0
	}
}

func formatDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	case d >= time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return strconv.Itoa(int(d.Minutes())) + "m"
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// windowAgg — mutable aggregator across multiple WindowSummary records
// ---------------------------------------------------------------------------

type windowAgg struct {
	totalAttempts     int64
	successAttempts   int64
	failedAttempts    int64
	excludedAttempts  int64
	totalDurationNs   int64
	totalFirstTokenNs int64
	firstTokenCount   int64
	windowSeconds     float64
	windowCount       int

	totalRequests   int64
	successRequests int64
	failedRequests  int64
	retryRequests   int64
	retryRecovered  int64

	taskExecCount   int64
	taskExecSuccess int64

	channelScoreSum float64
}

func (a *windowAgg) addWindow(w WindowSummary) {
	a.totalAttempts += w.TotalAttempts
	a.successAttempts += w.SuccessAttempts
	a.failedAttempts += w.FailedAttempts
	a.excludedAttempts += w.ExcludedAttempts
	a.totalDurationNs += w.TotalDurationNs
	a.totalFirstTokenNs += w.TotalFirstTokenNs
	a.firstTokenCount += w.FirstTokenCount
	a.windowSeconds += w.WindowEnd.Sub(w.WindowStart).Seconds()
	a.windowCount++

	a.totalRequests += w.TotalRequests
	a.successRequests += w.SuccessRequests
	a.failedRequests += w.FailedRequests
	a.retryRequests += w.RetryRequests
	a.retryRecovered += w.RetryRecovered

	a.taskExecCount += w.TaskExecCount
	a.taskExecSuccess += w.TaskExecSuccess

	a.channelScoreSum += w.ChannelScore
}

func (a *windowAgg) toCounters() StatsCounters {
	var recoveryRate float64
	if a.retryRequests > 0 {
		recoveryRate = float64(a.retryRecovered) / float64(a.retryRequests)
	}
	var avgDur, avgFT, avgExec float64
	if a.totalAttempts > 0 {
		avgDur = float64(a.totalDurationNs) / float64(a.totalAttempts) / 1e6
	}
	if a.firstTokenCount > 0 {
		avgFT = float64(a.totalFirstTokenNs) / float64(a.firstTokenCount) / 1e6
	}
	var tps float64
	if a.windowSeconds > 0 {
		tps = float64(a.totalAttempts) / a.windowSeconds
	}
	if a.taskExecCount > 0 {
		avgExec = float64(a.totalDurationNs) / float64(a.taskExecCount) / 1e6
	}
	return StatsCounters{
		TotalRequests:     a.totalRequests,
		SuccessRequests:   a.successRequests,
		FailedRequests:    a.failedRequests,
		TotalAttempts:     a.totalAttempts,
		SuccessAttempts:   a.successAttempts,
		FailedAttempts:    a.failedAttempts,
		ExcludedAttempts:  a.excludedAttempts,
		RetryRequests:     a.retryRequests,
		RetryRecovered:    a.retryRecovered,
		RecoveryRate:      recoveryRate,
		TPS:              tps,
		AvgDurationMs:    avgDur,
		AvgFirstTokenMs:  avgFT,
		TaskExecCount:    a.taskExecCount,
		TaskExecSuccess:  a.taskExecSuccess,
		AvgExecDurationMs: avgExec,
	}
}
