package service

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Sync scoring tests
// =============================================================================

func TestComputeChannelScore_SyncModel_AllSuccess(t *testing.T) {
	t.Parallel()
	s := WindowSummary{
		TotalAttempts:   100,
		AsyncAttempts:   0,
		SuccessAttempts: 100,
		AvgDurationMs:   300,
		AvgOutputTPS:    80,
	}
	score := ComputeChannelScore(s)
	assert.True(t, score >= 80, "100%% success + fast + high TPS should score high, got %f", score)
}

func TestComputeChannelScore_SyncModel_WithFailures(t *testing.T) {
	t.Parallel()
	s := WindowSummary{
		TotalAttempts:   100,
		AsyncAttempts:   0,
		SuccessAttempts: 70,
		FailedAttempts:  30,
		ErrorLevelDist:  [4]int64{0, 20, 10, 0},
		AvgDurationMs:   2500,
		AvgOutputTPS:    25,
	}
	score := ComputeChannelScore(s)
	assert.True(t, score > 0 && score < 80, "70%% success should give moderate score, got %f", score)
}

func TestComputeChannelScore_SyncModel_NoData(t *testing.T) {
	t.Parallel()
	s := WindowSummary{TotalAttempts: 0}
	score := ComputeChannelScore(s)
	assert.Equal(t, defaultBaselineScore, score, "no data should return baseline score (80), not 100")
}

func TestComputeChannelScore_SyncModel_AllExcluded(t *testing.T) {
	t.Parallel()
	s := WindowSummary{TotalAttempts: 10, ExcludedAttempts: 10}
	score := ComputeChannelScore(s)
	assert.Equal(t, defaultBaselineScore, score, "all excluded should return baseline score (80), not 100")
}

func TestComputeChannelScore_SyncUsesFirstTokenMs(t *testing.T) {
	t.Parallel()
	s := WindowSummary{
		TotalAttempts:   50,
		SuccessAttempts: 50,
		AvgDurationMs:   8000,
		AvgFirstTokenMs: 400,
		AvgOutputTPS:    60,
	}
	score := ComputeChannelScore(s)
	assert.True(t, score > 75, "fast first token should boost score despite slow total, got %f", score)
}

// =============================================================================
// Async scoring tests
// =============================================================================

func TestComputeChannelScore_AsyncModel_AllSuccess(t *testing.T) {
	t.Parallel()
	s := WindowSummary{
		TotalAttempts:     100,
		AsyncAttempts:     100,
		SuccessAttempts:   100,
		AvgDurationMs:     500,
		TaskExecCount:     80,
		TaskExecSuccess:   80,
		AvgExecDurationMs: 20000,
	}
	score := ComputeChannelScore(s)
	assert.True(t, score >= 80, "100%% async success should score high, got %f", score)
}

func TestComputeChannelScore_AsyncModel_ExecFailures(t *testing.T) {
	t.Parallel()
	s := WindowSummary{
		TotalAttempts:     100,
		AsyncAttempts:     100,
		SuccessAttempts:   100,
		AvgDurationMs:     1000,
		TaskExecCount:     100,
		TaskExecSuccess:   50,
		AvgExecDurationMs: 120000,
	}
	score := ComputeChannelScore(s)
	assert.True(t, score < 80, "50%% exec success should penalize, got %f", score)
}

func TestComputeChannelScore_AsyncDetection(t *testing.T) {
	t.Parallel()
	sAsync := WindowSummary{
		TotalAttempts: 10, AsyncAttempts: 6, SuccessAttempts: 10,
		AvgDurationMs: 500, TaskExecCount: 6, TaskExecSuccess: 6,
	}
	sSync := WindowSummary{
		TotalAttempts: 10, AsyncAttempts: 0, SuccessAttempts: 10,
		AvgDurationMs: 500, AvgOutputTPS: 60,
	}
	asyncScore := ComputeChannelScore(sAsync)
	syncScore := ComputeChannelScore(sSync)

	assert.True(t, asyncScore > 0 && asyncScore <= 100)
	assert.True(t, syncScore > 0 && syncScore <= 100)
}

// =============================================================================
// Baseline score and sparse-blending tests
// =============================================================================

func TestComputeChannelScore_SyncModel_BaselineDefault(t *testing.T) {
	t.Parallel()
	// No data → should return exactly the default baseline (80), not 100.
	s := WindowSummary{TotalAttempts: 0}
	score := ComputeChannelScore(s)
	assert.Equal(t, 80.0, score, "no-data channel should score 80 (baseline), not 100")
}

func TestComputeChannelScore_AsyncModel_NoData_ReturnsBaseline(t *testing.T) {
	t.Parallel()
	s := WindowSummary{TotalAttempts: 0, AsyncAttempts: 0}
	score := computeAsyncScore(s)
	assert.Equal(t, defaultBaselineScore, score,
		"async no-data should return baseline %.1f, got %f", defaultBaselineScore, score)
}

func TestComputeChannelScore_CustomBaselineScore(t *testing.T) {
	orig := GetScoreWeights()
	defer SetScoreWeights(orig)

	w := orig
	w.Sync.BaselineScore = 70.0
	w.Sync.SparseThreshold = 10
	SetScoreWeights(w)

	// No data → custom baseline
	s := WindowSummary{TotalAttempts: 0}
	score := ComputeChannelScore(s)
	assert.Equal(t, 70.0, score, "custom baseline 70 should be returned for no-data channel")
}

func TestComputeChannelScore_SparseBlending_PartialTrust(t *testing.T) {
	t.Parallel()
	orig := GetScoreWeights()
	defer SetScoreWeights(orig)

	w := orig
	w.Sync.BaselineScore = 80.0
	w.Sync.SparseThreshold = 10
	SetScoreWeights(w)

	// 5 out of 10 threshold: blend = 0.5
	// 100% success, fast speed → computed score near max
	// Expected: somewhere between 80 (baseline) and computed (high)
	s := WindowSummary{
		TotalAttempts:   5,
		AsyncAttempts:   0,
		SuccessAttempts: 5,
		AvgDurationMs:   300,
		AvgOutputTPS:    80,
	}
	score := ComputeChannelScore(s)
	assert.True(t, score > 80.0 && score <= 100.0,
		"sparse blended score should be between baseline and full score, got %f", score)
}

func TestComputeChannelScore_SparseBlending_1Attempt(t *testing.T) {
	t.Parallel()
	orig := GetScoreWeights()
	defer SetScoreWeights(orig)

	w := orig
	w.Sync.BaselineScore = 80.0
	w.Sync.SparseThreshold = 5
	SetScoreWeights(w)

	// 1 success out of 5 threshold: blend = 0.2
	// Score should be close to baseline (20% computed, 80% baseline)
	s := WindowSummary{
		TotalAttempts:   1,
		SuccessAttempts: 1,
		AvgDurationMs:   300,
		AvgOutputTPS:    80,
	}
	score := ComputeChannelScore(s)
	// blend=0.2: score ≈ 80*0.8 + high*0.2
	assert.True(t, score >= 80.0, "should be at least baseline since 100%% success, got %f", score)
	assert.True(t, score <= 100.0, "should not exceed 100, got %f", score)
}

func TestComputeChannelScore_FullData_NoBlending(t *testing.T) {
	t.Parallel()
	// With >= sparseThreshold (5) effective attempts, no blending occurs.
	s := WindowSummary{
		TotalAttempts:   100,
		SuccessAttempts: 100,
		AvgDurationMs:   300,
		AvgOutputTPS:    80,
	}
	score := ComputeChannelScore(s)
	assert.True(t, score >= 80, "full data should score high, got %f", score)
}

// =============================================================================
// Score weights configuration tests
// =============================================================================

func TestUpdateScoreWeightsFromJSON_Valid(t *testing.T) {
	orig := GetScoreWeights()
	defer SetScoreWeights(orig)

	jsonStr := `{
		"sync": {"base_weight": 50, "severity_max": 10, "recovery_weight": 5, "speed_weight": 20, "tps_weight": 15},
		"level_weights": [0, 2, 4, 8]
	}`
	err := UpdateScoreWeightsFromJSON(jsonStr)
	require.NoError(t, err)

	w := GetScoreWeights()
	assert.Equal(t, 50.0, w.Sync.BaseWeight)
	assert.Equal(t, 10.0, w.Sync.SeverityMax)
	assert.Equal(t, [4]float64{0, 2, 4, 8}, w.LevelWeights)
}

func TestUpdateScoreWeightsFromJSON_Empty_ResetsDefaults(t *testing.T) {
	orig := GetScoreWeights()
	defer SetScoreWeights(orig)

	_ = UpdateScoreWeightsFromJSON(`{"sync":{"base_weight":99}}`)
	assert.Equal(t, 99.0, GetScoreWeights().Sync.BaseWeight)

	_ = UpdateScoreWeightsFromJSON("")
	assert.Equal(t, defaultScoreWeights.Sync.BaseWeight, GetScoreWeights().Sync.BaseWeight)
}

func TestUpdateScoreWeightsFromJSON_Invalid(t *testing.T) {
	orig := GetScoreWeights()
	defer SetScoreWeights(orig)

	err := UpdateScoreWeightsFromJSON("{invalid}")
	assert.Error(t, err)
}

// =============================================================================
// Per-group tracker tests
// =============================================================================

func TestTracker_SingleGroup_Success(t *testing.T) {
	t.Parallel()
	c := newTestCollector(nil)
	SetRelayStatsCollector(c)
	defer SetRelayStatsCollector(&noopCollector{})

	operation_setting.SetRelayStatsEnabled(true)
	defer operation_setting.SetRelayStatsEnabled(false)

	tracker := NewRelayStatsTracker("req-1", RelayIdentity{
		Group: "default", ModelName: "gpt-4",
	}, false)

	tracker.TrackAttempt(&AttemptEvent{
		ChannelID: 1, ModelName: "gpt-4", Group: "default",
		Success: true, Duration: 200 * time.Millisecond,
	})
	tracker.Complete(true)

	counters := c.GetCounters()
	assert.Equal(t, int64(1), counters.TotalRequests)
	assert.Equal(t, int64(1), counters.SuccessRequests)
}

func TestTracker_CrossGroupRetry_GroupAFails_GroupBSucceeds(t *testing.T) {
	t.Parallel()
	c := newTestCollector(nil)
	SetRelayStatsCollector(c)
	defer SetRelayStatsCollector(&noopCollector{})

	operation_setting.SetRelayStatsEnabled(true)
	defer operation_setting.SetRelayStatsEnabled(false)

	tracker := NewRelayStatsTracker("req-2", RelayIdentity{
		Group: "groupA", ModelName: "gpt-4",
	}, false)

	// Group A: 2 failed attempts
	tracker.TrackAttempt(&AttemptEvent{
		ChannelID: 1, ModelName: "gpt-4", Group: "groupA",
		Success: false, StatusCode: 500, Duration: 2 * time.Second,
	})
	tracker.TrackAttempt(&AttemptEvent{
		ChannelID: 2, ModelName: "gpt-4", Group: "groupA",
		Success: false, StatusCode: 500, Duration: 1 * time.Second,
	})

	// Group B: 1 successful attempt
	tracker.TrackAttempt(&AttemptEvent{
		ChannelID: 3, ModelName: "gpt-4", Group: "groupB",
		Success: true, Duration: 2 * time.Second,
	})

	tracker.Complete(true)

	// 2 RequestCompleteEvents: groupA=failed, groupB=success
	counters := c.GetCounters()
	assert.Equal(t, int64(2), counters.TotalRequests, "two groups = two request events")
	assert.Equal(t, int64(1), counters.SuccessRequests, "only groupB succeeded")
	assert.Equal(t, int64(1), counters.FailedRequests, "groupA failed")

	flushWindows(c)
	summaries := c.GetWindowSummaries(100)

	groupAReq, groupBReq := false, false
	for _, s := range summaries {
		if s.Group == "groupA" && s.TotalRequests > 0 {
			groupAReq = true
			assert.Equal(t, int64(1), s.FailedRequests)
			assert.Equal(t, int64(0), s.SuccessRequests)
		}
		if s.Group == "groupB" && s.TotalRequests > 0 {
			groupBReq = true
			assert.Equal(t, int64(1), s.SuccessRequests)
		}
	}
	assert.True(t, groupAReq, "should have groupA request data")
	assert.True(t, groupBReq, "should have groupB request data")
}

func TestTracker_CrossGroupRetry_AllFail(t *testing.T) {
	t.Parallel()
	c := newTestCollector(nil)
	SetRelayStatsCollector(c)
	defer SetRelayStatsCollector(&noopCollector{})

	operation_setting.SetRelayStatsEnabled(true)
	defer operation_setting.SetRelayStatsEnabled(false)

	tracker := NewRelayStatsTracker("req-3", RelayIdentity{
		Group: "groupA", ModelName: "gpt-4",
	}, false)

	tracker.TrackAttempt(&AttemptEvent{
		ChannelID: 1, ModelName: "gpt-4", Group: "groupA",
		Success: false, StatusCode: 500,
	})
	tracker.TrackAttempt(&AttemptEvent{
		ChannelID: 2, ModelName: "gpt-4", Group: "groupB",
		Success: false, StatusCode: 500,
	})
	tracker.Complete(false)

	counters := c.GetCounters()
	assert.Equal(t, int64(2), counters.TotalRequests)
	assert.Equal(t, int64(0), counters.SuccessRequests)
	assert.Equal(t, int64(2), counters.FailedRequests, "both groups failed")
}

// =============================================================================
// TPS (output tokens per second) tests
// =============================================================================

func TestWindowBuffer_OutputTPS(t *testing.T) {
	t.Parallel()
	wb := NewWindowBuffer(nil, time.Hour, nil)
	defer wb.Stop()

	for i := 0; i < 5; i++ {
		wb.CollectAttempt(&AttemptEvent{
			ModelName: "gpt-4", ChannelID: 1,
			Success: true, Duration: 1 * time.Second, CompletionTokens: 100,
		})
	}

	summaries := wb.Flush()
	require.Len(t, summaries, 1)

	s := summaries[0]
	assert.Equal(t, int64(500), s.TotalCompletionTokens)
	assert.InDelta(t, 100.0, s.AvgOutputTPS, 1.0, "500 tokens / 5s = 100 tps")
}

func TestWindowBuffer_OutputTPS_OnlySuccessful(t *testing.T) {
	t.Parallel()
	wb := NewWindowBuffer(nil, time.Hour, nil)
	defer wb.Stop()

	wb.CollectAttempt(&AttemptEvent{
		ModelName: "gpt-4", ChannelID: 1,
		Success: true, Duration: 2 * time.Second, CompletionTokens: 200,
	})
	wb.CollectAttempt(&AttemptEvent{
		ModelName: "gpt-4", ChannelID: 1,
		Success: true, Duration: 1 * time.Second, CompletionTokens: 100,
	})
	wb.CollectAttempt(&AttemptEvent{
		ModelName: "gpt-4", ChannelID: 1,
		Success: false, StatusCode: 500, Duration: 10 * time.Second,
	})

	summaries := wb.Flush()
	require.Len(t, summaries, 1)
	assert.Equal(t, int64(300), summaries[0].TotalCompletionTokens)
	assert.InDelta(t, 100.0, summaries[0].AvgOutputTPS, 1.0,
		"300 tokens / 3s = 100 tps (failed attempt excluded)")
}

func TestWindowBuffer_AsyncAttempts(t *testing.T) {
	t.Parallel()
	wb := NewWindowBuffer(nil, time.Hour, nil)
	defer wb.Stop()

	wb.CollectAttempt(&AttemptEvent{ModelName: "mj", ChannelID: 1, IsAsync: true, Success: true})
	wb.CollectAttempt(&AttemptEvent{ModelName: "mj", ChannelID: 1, IsAsync: true, Success: false, StatusCode: 500})
	wb.CollectAttempt(&AttemptEvent{ModelName: "mj", ChannelID: 1, IsAsync: false, Success: true})

	summaries := wb.Flush()
	require.Len(t, summaries, 1)
	assert.Equal(t, int64(3), summaries[0].TotalAttempts)
	assert.Equal(t, int64(2), summaries[0].AsyncAttempts)
}

// =============================================================================
// Response time isolation (per-channel, no retry contamination)
// =============================================================================

func TestResponseTime_PerChannel_NoRetryCross(t *testing.T) {
	t.Parallel()
	wb := NewWindowBuffer(nil, time.Hour, nil)
	defer wb.Stop()

	// Channel A: failed, 2 seconds
	wb.CollectAttempt(&AttemptEvent{
		ModelName: "gpt-4", ChannelID: 10, Group: "default",
		Success: false, StatusCode: 500, Duration: 2 * time.Second,
	})
	// Channel B: success, 2 seconds
	wb.CollectAttempt(&AttemptEvent{
		ModelName: "gpt-4", ChannelID: 20, Group: "default",
		Success: true, Duration: 2 * time.Second,
	})

	summaries := wb.Flush()
	require.Len(t, summaries, 2, "separate channels → separate summaries")

	for _, s := range summaries {
		assert.InDelta(t, 2000.0, s.AvgDurationMs, 10.0,
			"channel %d should have 2s avg, not combined 4s", s.ChannelID)
	}
}

// =============================================================================
// RelayStats enabled guard test
// =============================================================================

func TestSafeCollect_DisabledByDefault(t *testing.T) {
	t.Parallel()
	c := newTestCollector(nil)
	SetRelayStatsCollector(c)
	defer SetRelayStatsCollector(&noopCollector{})

	// Default is disabled
	operation_setting.SetRelayStatsEnabled(false)

	SafeCollectAttempt(c, &AttemptEvent{Success: true})
	SafeCollectRequestComplete(c, RequestCompleteEvent{FinalSuccess: true})

	counters := c.GetCounters()
	assert.Equal(t, int64(0), counters.TotalAttempts, "disabled should not collect")
	assert.Equal(t, int64(0), counters.TotalRequests, "disabled should not collect")
}

func TestSafeCollect_Enabled(t *testing.T) {
	t.Parallel()
	c := newTestCollector(nil)
	SetRelayStatsCollector(c)
	defer SetRelayStatsCollector(&noopCollector{})

	operation_setting.SetRelayStatsEnabled(true)
	defer operation_setting.SetRelayStatsEnabled(false)

	SafeCollectAttempt(c, &AttemptEvent{Success: true})
	SafeCollectRequestComplete(c, RequestCompleteEvent{FinalSuccess: true})

	counters := c.GetCounters()
	assert.Equal(t, int64(1), counters.TotalAttempts, "enabled should collect")
	assert.Equal(t, int64(1), counters.TotalRequests, "enabled should collect")
}
