package service

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// RingBuffer tests
// =============================================================================

func TestRingBuffer_PushAndSnapshot(t *testing.T) {
	t.Parallel()
	rb := NewRingBuffer[int](5)

	rb.Push(1)
	rb.Push(2)
	rb.Push(3)

	got := rb.Snapshot(0)
	require.Equal(t, []int{1, 2, 3}, got)
}

func TestRingBuffer_SnapshotWithLimit(t *testing.T) {
	t.Parallel()
	rb := NewRingBuffer[int](5)
	for i := 1; i <= 5; i++ {
		rb.Push(i)
	}

	got := rb.Snapshot(3)
	require.Equal(t, []int{3, 4, 5}, got, "should return the 3 most recent items")
}

func TestRingBuffer_Overflow(t *testing.T) {
	t.Parallel()
	rb := NewRingBuffer[int](3)
	for i := 1; i <= 7; i++ {
		rb.Push(i)
	}

	got := rb.Snapshot(0)
	require.Equal(t, []int{5, 6, 7}, got, "oldest entries should be overwritten")
}

func TestRingBuffer_SnapshotEmpty(t *testing.T) {
	t.Parallel()
	rb := NewRingBuffer[int](5)
	require.Nil(t, rb.Snapshot(0))
	require.Nil(t, rb.Snapshot(10))
}

func TestRingBuffer_Reset(t *testing.T) {
	t.Parallel()
	rb := NewRingBuffer[int](5)
	rb.Push(1)
	rb.Push(2)
	rb.Reset()

	require.Nil(t, rb.Snapshot(0))
}

func TestRingBuffer_DefaultCapacity(t *testing.T) {
	t.Parallel()
	rb := NewRingBuffer[int](0)
	require.Equal(t, defaultRingBufferSize, rb.cap)

	rb2 := NewRingBuffer[int](-1)
	require.Equal(t, defaultRingBufferSize, rb2.cap)
}

func TestRingBuffer_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	rb := NewRingBuffer[int](100)
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				rb.Push(base*50 + j)
			}
		}(i)
	}

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				rb.Snapshot(10)
			}
		}()
	}

	wg.Wait()
	got := rb.Snapshot(0)
	require.Equal(t, 100, len(got), "buffer should be full after 500 pushes into cap=100")
}

// =============================================================================
// Helper: create collector and flush windows for testing
// =============================================================================

func newTestCollector(classifier ErrorClassifier) *MemoryStatsCollector {
	c := NewMemoryStatsCollector(classifier, time.Hour, 100)
	return c
}

// flushAndGet triggers a manual flush and returns the collector for further assertions.
func flushWindows(c *MemoryStatsCollector) {
	summaries := c.windowBuf.Flush()
	c.summaries.PushBatch(summaries)
}

// =============================================================================
// MemoryStatsCollector tests
// =============================================================================

func TestCollector_SuccessAttempt(t *testing.T) {
	t.Parallel()
	c := newTestCollector(nil)

	c.CollectAttempt(&AttemptEvent{
		RequestID: "r1",
		ChannelID: 1,
		Success:   true,
	})

	counters := c.GetCounters()
	assert.Equal(t, int64(1), counters.TotalAttempts)
	assert.Equal(t, int64(1), counters.SuccessAttempts)
	assert.Equal(t, int64(0), counters.FailedAttempts)
	assert.Equal(t, int64(0), counters.ExcludedAttempts)
}

func TestCollector_FailedAttempt_NoClassifier(t *testing.T) {
	t.Parallel()
	c := newTestCollector(nil)

	c.CollectAttempt(&AttemptEvent{
		RequestID:  "r1",
		ChannelID:  1,
		Success:    false,
		StatusCode: 500,
		ErrorCode:  "internal_error",
	})

	counters := c.GetCounters()
	assert.Equal(t, int64(1), counters.TotalAttempts)
	assert.Equal(t, int64(0), counters.SuccessAttempts)
	assert.Equal(t, int64(1), counters.FailedAttempts)
	assert.Equal(t, int64(0), counters.ExcludedAttempts)
}

func TestCollector_FailedAttempt_Excluded(t *testing.T) {
	t.Parallel()
	classifier := NewRuleBasedClassifier([]ErrorExclusionRule{
		{StatusCodes: []int{400}, Description: "client error"},
	})
	c := newTestCollector(classifier)

	c.CollectAttempt(&AttemptEvent{
		RequestID:  "r1",
		ChannelID:  1,
		Success:    false,
		StatusCode: 400,
		ErrorCode:  "bad_request",
	})

	counters := c.GetCounters()
	assert.Equal(t, int64(1), counters.TotalAttempts)
	assert.Equal(t, int64(0), counters.FailedAttempts, "excluded errors should not count as failed")
	assert.Equal(t, int64(1), counters.ExcludedAttempts)
}

func TestCollector_RequestComplete_Success(t *testing.T) {
	t.Parallel()
	c := newTestCollector(nil)

	c.CollectRequestComplete(RequestCompleteEvent{
		RequestID:     "r1",
		TotalAttempts: 1,
		FinalSuccess:  true,
	})

	counters := c.GetCounters()
	assert.Equal(t, int64(1), counters.TotalRequests)
	assert.Equal(t, int64(1), counters.SuccessRequests)
	assert.Equal(t, int64(0), counters.FailedRequests)
	assert.Equal(t, int64(0), counters.RetryRequests)
	assert.Equal(t, int64(0), counters.RetryRecovered)
}

func TestCollector_RequestComplete_RetryRecovered(t *testing.T) {
	t.Parallel()
	c := newTestCollector(nil)

	c.CollectRequestComplete(RequestCompleteEvent{
		RequestID:      "r1",
		TotalAttempts:  3,
		FinalSuccess:   true,
		HasRetry:       true,
		RetryRecovered: true,
	})

	counters := c.GetCounters()
	assert.Equal(t, int64(1), counters.TotalRequests)
	assert.Equal(t, int64(1), counters.SuccessRequests)
	assert.Equal(t, int64(1), counters.RetryRequests)
	assert.Equal(t, int64(1), counters.RetryRecovered)
	assert.Equal(t, 1.0, counters.RecoveryRate)
}

func TestCollector_RequestComplete_RetryFailed(t *testing.T) {
	t.Parallel()
	c := newTestCollector(nil)

	c.CollectRequestComplete(RequestCompleteEvent{
		RequestID:     "r1",
		TotalAttempts: 3,
		FinalSuccess:  false,
		HasRetry:      true,
	})

	counters := c.GetCounters()
	assert.Equal(t, int64(1), counters.TotalRequests)
	assert.Equal(t, int64(0), counters.SuccessRequests)
	assert.Equal(t, int64(1), counters.FailedRequests)
	assert.Equal(t, int64(1), counters.RetryRequests)
	assert.Equal(t, int64(0), counters.RetryRecovered)
	assert.Equal(t, 0.0, counters.RecoveryRate)
}

func TestCollector_RecoveryRate(t *testing.T) {
	t.Parallel()
	c := newTestCollector(nil)

	for i := 0; i < 3; i++ {
		c.CollectRequestComplete(RequestCompleteEvent{
			HasRetry: true, RetryRecovered: true, FinalSuccess: true,
		})
	}
	c.CollectRequestComplete(RequestCompleteEvent{
		HasRetry: true, FinalSuccess: false,
	})

	counters := c.GetCounters()
	assert.Equal(t, int64(4), counters.RetryRequests)
	assert.Equal(t, int64(3), counters.RetryRecovered)
	assert.InDelta(t, 0.75, counters.RecoveryRate, 0.001)
}

func TestCollector_Reset(t *testing.T) {
	t.Parallel()
	c := newTestCollector(nil)

	c.CollectAttempt(&AttemptEvent{Success: true})
	c.CollectAttempt(&AttemptEvent{Success: false, StatusCode: 500})
	c.CollectRequestComplete(RequestCompleteEvent{FinalSuccess: true})

	c.Reset()

	counters := c.GetCounters()
	assert.Equal(t, int64(0), counters.TotalAttempts)
	assert.Equal(t, int64(0), counters.TotalRequests)
	assert.Nil(t, c.GetWindowSummaries(10))
}

func TestCollector_MixedScenario(t *testing.T) {
	t.Parallel()
	classifier := NewRuleBasedClassifier([]ErrorExclusionRule{
		{StatusCodes: []int{400}, Description: "client_error"},
		{ErrorCodes: []string{"rate_limited"}, Description: "rate_limit"},
	})
	c := newTestCollector(classifier)

	c.CollectAttempt(&AttemptEvent{Success: false, StatusCode: 400})
	c.CollectAttempt(&AttemptEvent{Success: false, StatusCode: 500})
	c.CollectAttempt(&AttemptEvent{Success: false, ErrorCode: "rate_limited"})
	c.CollectAttempt(&AttemptEvent{Success: true})

	counters := c.GetCounters()
	assert.Equal(t, int64(4), counters.TotalAttempts)
	assert.Equal(t, int64(1), counters.SuccessAttempts)
	assert.Equal(t, int64(1), counters.FailedAttempts, "only the 500 error is a real failure")
	assert.Equal(t, int64(2), counters.ExcludedAttempts, "400 + rate_limited are excluded")
}

// =============================================================================
// WindowBuffer + AggregateWindows tests
// =============================================================================

func TestAggregateWindows_ByModel(t *testing.T) {
	t.Parallel()
	c := newTestCollector(nil)

	c.CollectAttempt(&AttemptEvent{ModelName: "gpt-4", Success: true, Duration: 100 * time.Millisecond})
	c.CollectAttempt(&AttemptEvent{ModelName: "gpt-4", Success: true, Duration: 200 * time.Millisecond})
	c.CollectAttempt(&AttemptEvent{ModelName: "gpt-4", Success: false, StatusCode: 500, Duration: 50 * time.Millisecond})
	c.CollectAttempt(&AttemptEvent{ModelName: "claude-3", Success: true, Duration: 150 * time.Millisecond})
	c.CollectAttempt(&AttemptEvent{ModelName: "claude-3", Success: false, StatusCode: 429, Duration: 30 * time.Millisecond})

	flushWindows(c)

	result := c.AggregateWindows([]string{"model"})
	require.Len(t, result, 2)

	gpt4 := result["gpt-4"]
	assert.Equal(t, int64(3), gpt4.TotalAttempts)
	assert.Equal(t, int64(2), gpt4.SuccessAttempts)
	assert.Equal(t, int64(1), gpt4.FailedAttempts)

	claude := result["claude-3"]
	assert.Equal(t, int64(2), claude.TotalAttempts)
	assert.Equal(t, int64(1), claude.SuccessAttempts)
	assert.Equal(t, int64(1), claude.FailedAttempts)
}

func TestAggregateWindows_ByChannel(t *testing.T) {
	t.Parallel()
	c := newTestCollector(nil)

	c.CollectAttempt(&AttemptEvent{ModelName: "m1", ChannelID: 10, Success: true})
	c.CollectAttempt(&AttemptEvent{ModelName: "m1", ChannelID: 10, Success: false, StatusCode: 500})
	c.CollectAttempt(&AttemptEvent{ModelName: "m1", ChannelID: 20, Success: true})

	flushWindows(c)

	result := c.AggregateWindows([]string{"channel"})
	require.Len(t, result, 2)
	assert.Equal(t, int64(2), result["10"].TotalAttempts)
	assert.Equal(t, int64(1), result["20"].TotalAttempts)
}

func TestAggregateWindows_WithExclusion(t *testing.T) {
	t.Parallel()
	classifier := NewRuleBasedClassifier([]ErrorExclusionRule{
		{StatusCodes: []int{400}, Description: "client_error"},
	})
	c := newTestCollector(classifier)

	c.CollectAttempt(&AttemptEvent{ModelName: "gpt-4", Success: false, StatusCode: 400})
	c.CollectAttempt(&AttemptEvent{ModelName: "gpt-4", Success: false, StatusCode: 500})
	c.CollectAttempt(&AttemptEvent{ModelName: "gpt-4", Success: true})

	flushWindows(c)

	result := c.AggregateWindows([]string{"model"})
	gpt4 := result["gpt-4"]
	assert.Equal(t, int64(3), gpt4.TotalAttempts)
	assert.Equal(t, int64(1), gpt4.SuccessAttempts)
	assert.Equal(t, int64(1), gpt4.FailedAttempts)
	assert.Equal(t, int64(1), gpt4.ExcludedAttempts)
}

func TestAggregateWindows_InvalidDimension(t *testing.T) {
	t.Parallel()
	c := newTestCollector(nil)
	c.CollectAttempt(&AttemptEvent{Success: true})
	flushWindows(c)

	result := c.AggregateWindows([]string{"nonexistent"})
	assert.Nil(t, result)
}

func TestAggregateWindows_Requests(t *testing.T) {
	t.Parallel()
	c := newTestCollector(nil)

	c.CollectRequestComplete(RequestCompleteEvent{OriginalModel: "gpt-4", FinalSuccess: true, TotalAttempts: 1})
	c.CollectRequestComplete(RequestCompleteEvent{OriginalModel: "gpt-4", FinalSuccess: false, HasRetry: true, TotalAttempts: 3})
	c.CollectRequestComplete(RequestCompleteEvent{OriginalModel: "gpt-4", FinalSuccess: true, HasRetry: true, RetryRecovered: true, TotalAttempts: 2})
	c.CollectRequestComplete(RequestCompleteEvent{OriginalModel: "claude-3", FinalSuccess: true, TotalAttempts: 1})

	flushWindows(c)

	result := c.AggregateWindows([]string{"model"})
	require.Len(t, result, 2)

	gpt4 := result["gpt-4"]
	assert.Equal(t, int64(3), gpt4.TotalRequests)
	assert.Equal(t, int64(2), gpt4.SuccessRequests)
	assert.Equal(t, int64(1), gpt4.FailedRequests)
	assert.Equal(t, int64(2), gpt4.RetryRequests)
	assert.Equal(t, int64(1), gpt4.RetryRecovered)

	claude := result["claude-3"]
	assert.Equal(t, int64(1), claude.TotalRequests)
	assert.Equal(t, int64(1), claude.SuccessRequests)
}

// =============================================================================
// TaskExecution tests
// =============================================================================

func TestCollector_TaskExecution(t *testing.T) {
	t.Parallel()
	c := newTestCollector(nil)

	c.CollectTaskExecution(TaskExecutionEvent{
		TaskID:            "task-1",
		ModelName:         "midjourney",
		ChannelID:         5,
		Success:           true,
		ExecutionDuration: 30 * time.Second,
	})
	c.CollectTaskExecution(TaskExecutionEvent{
		TaskID:            "task-2",
		ModelName:         "midjourney",
		ChannelID:         5,
		Success:           false,
		FailReason:        "upstream timeout",
		ExecutionDuration: 60 * time.Second,
	})

	counters := c.GetCounters()
	assert.Equal(t, int64(2), counters.TaskExecCount)
	assert.Equal(t, int64(1), counters.TaskExecSuccess)
}

func TestCollector_AsyncSubmitTracking(t *testing.T) {
	t.Parallel()
	c := newTestCollector(nil)

	c.CollectAttempt(&AttemptEvent{IsAsync: true, Success: true})
	c.CollectAttempt(&AttemptEvent{IsAsync: true, Success: false, StatusCode: 500})
	c.CollectAttempt(&AttemptEvent{IsAsync: false, Success: true})

	counters := c.GetCounters()
	assert.Equal(t, int64(2), counters.TaskSubmitCount)
	assert.Equal(t, int64(1), counters.TaskSubmitSuccess)
}

// =============================================================================
// WindowSummary tests
// =============================================================================

func TestWindowBuffer_Flush(t *testing.T) {
	t.Parallel()
	wb := NewWindowBuffer(nil, time.Hour, nil)
	defer wb.Stop()

	wb.CollectAttempt(&AttemptEvent{ModelName: "gpt-4", ChannelID: 1, Success: true, Duration: 100 * time.Millisecond})
	wb.CollectAttempt(&AttemptEvent{ModelName: "gpt-4", ChannelID: 1, Success: false, StatusCode: 500, Duration: 50 * time.Millisecond})
	// Request-complete events have no ChannelID (span multiple channels) — produces a separate bucket
	wb.CollectRequestComplete(&RequestCompleteEvent{OriginalModel: "gpt-4", FinalSuccess: true})

	summaries := wb.Flush()
	require.True(t, len(summaries) >= 1)

	// Find the attempt-level summary (ChannelID=1)
	var attemptSummary *WindowSummary
	var requestSummary *WindowSummary
	for i := range summaries {
		if summaries[i].ChannelID == 1 {
			attemptSummary = &summaries[i]
		}
		if summaries[i].TotalRequests > 0 {
			requestSummary = &summaries[i]
		}
	}
	require.NotNil(t, attemptSummary)
	assert.Equal(t, "gpt-4", attemptSummary.ModelName)
	assert.Equal(t, int64(2), attemptSummary.TotalAttempts)
	assert.Equal(t, int64(1), attemptSummary.SuccessAttempts)
	assert.Equal(t, int64(1), attemptSummary.FailedAttempts)
	assert.True(t, attemptSummary.TPS > 0)
	assert.True(t, attemptSummary.AvgDurationMs > 0)

	require.NotNil(t, requestSummary)
	assert.Equal(t, int64(1), requestSummary.TotalRequests)
	assert.Equal(t, int64(1), requestSummary.SuccessRequests)
}

func TestWindowBuffer_ChannelScore(t *testing.T) {
	t.Parallel()
	wb := NewWindowBuffer(nil, time.Hour, nil)
	defer wb.Stop()

	// All successful
	for i := 0; i < 100; i++ {
		wb.CollectAttempt(&AttemptEvent{ModelName: "gpt-4", ChannelID: 1, Success: true, Duration: 200 * time.Millisecond})
	}

	summaries := wb.Flush()
	require.Len(t, summaries, 1)
	assert.True(t, summaries[0].ChannelScore > 60, "100%% success should give high score")
}

// =============================================================================
// TimeSeries tests
// =============================================================================

func TestGetTimeSeries_Basic(t *testing.T) {
	t.Parallel()
	c := newTestCollector(nil)

	c.CollectAttempt(&AttemptEvent{ModelName: "gpt-4", ChannelID: 1, Success: true})
	c.CollectAttempt(&AttemptEvent{ModelName: "gpt-4", ChannelID: 1, Success: false, StatusCode: 500})
	c.CollectAttempt(&AttemptEvent{ModelName: "claude-3", ChannelID: 2, Success: true})

	flushWindows(c)

	result := c.GetTimeSeries(TimeSeriesQuery{
		GroupBy:  "model",
		Metric:   "success_rate",
		Interval: 5 * time.Minute,
		Range:    time.Hour,
	})

	assert.Equal(t, "success_rate", result.Metric)
	assert.Equal(t, "model", result.GroupBy)
	assert.True(t, len(result.Series) > 0, "should have at least one series")
}

// =============================================================================
// ModelStats pointer fields and HasData tests
// =============================================================================

func TestGetModelStats_WithData_PointerFieldsSet(t *testing.T) {
	t.Parallel()
	c := newTestCollector(nil)

	// Provide 5 successful attempts with duration so avg_duration is computed
	for i := 0; i < 5; i++ {
		c.CollectAttempt(&AttemptEvent{
			ModelName: "gpt-4", ChannelID: 1,
			Success: true, Duration: 1 * time.Second, CompletionTokens: 100,
		})
	}
	flushWindows(c)

	stats := c.GetModelStats(0, 0)
	require.Len(t, stats, 1)
	s := stats[0]

	assert.Equal(t, "gpt-4", s.ModelName)
	assert.True(t, s.HasData, "HasData should be true when data exists")
	assert.NotNil(t, s.AvgDurationMs, "AvgDurationMs should be non-nil when attempts exist")
	assert.NotNil(t, s.TPS, "TPS should be non-nil when completion tokens exist")
	assert.InDelta(t, 100.0, s.SuccessRate, 0.1, "100%% success")
}

func TestGetModelStats_WithFirstTokenData(t *testing.T) {
	t.Parallel()
	c := newTestCollector(nil)

	c.CollectAttempt(&AttemptEvent{
		ModelName: "claude-3", ChannelID: 1,
		Success: true, Duration: 2 * time.Second, FirstTokenDuration: 500 * time.Millisecond,
	})
	flushWindows(c)

	stats := c.GetModelStats(0, 0)
	require.Len(t, stats, 1)
	s := stats[0]

	assert.NotNil(t, s.AvgFirstTokenMs, "AvgFirstTokenMs should be non-nil when first-token data exists")
	assert.InDelta(t, 500.0, *s.AvgFirstTokenMs, 10.0, "first token ms should be ~500")
}

func TestGetModelStats_NoFirstToken_NilFirstTokenField(t *testing.T) {
	t.Parallel()
	c := newTestCollector(nil)

	// Attempt with no first-token duration (non-streaming)
	c.CollectAttempt(&AttemptEvent{
		ModelName: "gpt-3.5", ChannelID: 1,
		Success: true, Duration: 1 * time.Second,
	})
	flushWindows(c)

	stats := c.GetModelStats(0, 0)
	require.Len(t, stats, 1)
	assert.Nil(t, stats[0].AvgFirstTokenMs, "AvgFirstTokenMs should be nil when no streaming data")
}

func TestGetModelStats_NoAttempts_NilDuration(t *testing.T) {
	t.Parallel()
	c := newTestCollector(nil)

	// Collect a request complete event without any attempt (edge case)
	c.CollectRequestComplete(RequestCompleteEvent{
		FinalSuccess: true, TotalAttempts: 0,
	})
	flushWindows(c)

	// No model data → empty result
	stats := c.GetModelStats(0, 0)
	assert.Empty(t, stats, "no model data should return empty stats")
}

func TestGetModelStats_HasData_False_ForZeroTrafficModels(t *testing.T) {
	// When constructing a zero-traffic ModelStats entry (as the controller does),
	// HasData should be false and pointer fields should be nil.
	zero := ModelStats{
		ModelName:   "some-model",
		SuccessRate: 100,
		HasData:     false,
	}
	assert.False(t, zero.HasData)
	assert.Nil(t, zero.AvgDurationMs)
	assert.Nil(t, zero.AvgFirstTokenMs)
	assert.Nil(t, zero.TPS)
	assert.Equal(t, 100.0, zero.SuccessRate)
}
