package service

import (
	"math"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
)

// ScoreWeights holds all configurable parameters for ComputeChannelScore.
// Sync and Async models use different scoring formulas.
type ScoreWeights struct {
	Sync         SyncScoreConfig  `json:"sync"`
	Async        AsyncScoreConfig `json:"async"`
	LevelWeights [4]float64       `json:"level_weights"`
}

// SyncScoreConfig weights for synchronous (chat/completion) models.
// Factors: success rate, error severity, TPS, first-token speed, recovery.
//
// BaselineScore is the score returned when a channel has no effective data
// (effective attempts == 0). It is also blended with the computed score when
// data is very sparse (< SparseThreshold attempts). Default 80.
type SyncScoreConfig struct {
	BaseWeight      float64        `json:"base_weight"`
	SeverityMax     float64        `json:"severity_max"`
	RecoveryWeight  float64        `json:"recovery_weight"`
	SpeedWeight     float64        `json:"speed_weight"`
	TPSWeight       float64        `json:"tps_weight"`
	SpeedThresholds SpeedThreshold `json:"speed_thresholds"`
	TPSThresholds   TPSThreshold   `json:"tps_thresholds"`
	BaselineScore   float64        `json:"baseline_score"`
	SparseThreshold int64          `json:"sparse_threshold"`
}

// AsyncScoreConfig weights for asynchronous (task) models.
// Factors: submit success rate, submit speed, exec success rate, exec speed, recovery.
//
// BaselineScore and SparseThreshold work the same as in SyncScoreConfig.
type AsyncScoreConfig struct {
	SubmitBaseWeight      float64        `json:"submit_base_weight"`
	SeverityMax           float64        `json:"severity_max"`
	RecoveryWeight        float64        `json:"recovery_weight"`
	SubmitSpeedWeight     float64        `json:"submit_speed_weight"`
	ExecSuccessWeight     float64        `json:"exec_success_weight"`
	ExecSpeedWeight       float64        `json:"exec_speed_weight"`
	SubmitSpeedThresholds SpeedThreshold `json:"submit_speed_thresholds"`
	ExecSpeedThresholds   SpeedThreshold `json:"exec_speed_thresholds"`
	BaselineScore         float64        `json:"baseline_score"`
	SparseThreshold       int64          `json:"sparse_threshold"`
}

type SpeedThreshold struct {
	ExcellentMs float64 `json:"excellent_ms"`
	GoodMs      float64 `json:"good_ms"`
	OkMs        float64 `json:"ok_ms"`
	SlowMs      float64 `json:"slow_ms"`
}

type TPSThreshold struct {
	Excellent float64 `json:"excellent"`
	Good      float64 `json:"good"`
	Ok        float64 `json:"ok"`
	Slow      float64 `json:"slow"`
}

// defaultBaselineScore is the channel score returned when there is no effective
// data, and the anchor value used for blending with sparse-data computed scores.
const defaultBaselineScore = 80.0

// defaultSparseThreshold is the minimum number of effective attempts required
// before the computed score is trusted fully (no blending with baseline).
const defaultSparseThreshold int64 = 5

var defaultScoreWeights = ScoreWeights{
	Sync: SyncScoreConfig{
		BaseWeight:      40.0,
		SeverityMax:     15.0,
		RecoveryWeight:  5.0,
		SpeedWeight:     25.0,
		TPSWeight:       30.0,
		BaselineScore:   defaultBaselineScore,
		SparseThreshold: defaultSparseThreshold,
		SpeedThresholds: SpeedThreshold{
			ExcellentMs: 500,
			GoodMs:      2000,
			OkMs:        5000,
			SlowMs:      10000,
		},
		TPSThresholds: TPSThreshold{
			Excellent: 100,
			Good:      50,
			Ok:        20,
			Slow:      5,
		},
	},
	Async: AsyncScoreConfig{
		SubmitBaseWeight:  30.0,
		SeverityMax:       10.0,
		RecoveryWeight:    5.0,
		SubmitSpeedWeight: 15.0,
		ExecSuccessWeight: 25.0,
		ExecSpeedWeight:   25.0,
		BaselineScore:     defaultBaselineScore,
		SparseThreshold:   defaultSparseThreshold,
		SubmitSpeedThresholds: SpeedThreshold{
			ExcellentMs: 1000,
			GoodMs:      3000,
			OkMs:        8000,
			SlowMs:      15000,
		},
		ExecSpeedThresholds: SpeedThreshold{
			ExcellentMs: 30000,
			GoodMs:      60000,
			OkMs:        180000,
			SlowMs:      600000,
		},
	},
	LevelWeights: [4]float64{0, 1, 3, 6},
}

var (
	scoreWeightsMu sync.RWMutex
	activeWeights  = defaultScoreWeights
)

func GetScoreWeights() ScoreWeights {
	scoreWeightsMu.RLock()
	defer scoreWeightsMu.RUnlock()
	return activeWeights
}

func SetScoreWeights(w ScoreWeights) {
	scoreWeightsMu.Lock()
	activeWeights = w
	scoreWeightsMu.Unlock()
}

func UpdateScoreWeightsFromJSON(jsonStr string) error {
	if strings.TrimSpace(jsonStr) == "" {
		SetScoreWeights(defaultScoreWeights)
		return nil
	}
	w := defaultScoreWeights
	if err := common.Unmarshal([]byte(jsonStr), &w); err != nil {
		return err
	}
	SetScoreWeights(w)
	return nil
}

func ScoreWeightsToJSON() string {
	w := GetScoreWeights()
	data, err := common.Marshal(w)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// ComputeChannelScore calculates a 0-100 health score for a window summary.
// Automatically selects sync or async scoring based on whether the majority
// of attempts in the window are async (task submits).
func ComputeChannelScore(s WindowSummary) float64 {
	if s.AsyncAttempts > 0 && s.AsyncAttempts*2 >= s.TotalAttempts {
		return computeAsyncScore(s)
	}
	return computeSyncScore(s)
}

// syncBaseline returns the configured baseline score, defaulting to
// defaultBaselineScore when the config value is zero (unset).
func syncBaseline(sc SyncScoreConfig) float64 {
	if sc.BaselineScore > 0 {
		return sc.BaselineScore
	}
	return defaultBaselineScore
}

// syncSparseThreshold returns the configured sparse threshold, defaulting to
// defaultSparseThreshold when the config value is zero (unset).
func syncSparseThreshold(sc SyncScoreConfig) int64 {
	if sc.SparseThreshold > 0 {
		return sc.SparseThreshold
	}
	return defaultSparseThreshold
}

// computeSyncScore evaluates: success rate, TPS, first-token/response speed, recovery.
// AvgDurationMs is per-attempt per-channel, so retry across channels does not inflate it.
//
// When effective attempts == 0 the configured BaselineScore (default 80) is returned
// instead of 100, because an untested channel should not start at perfect.
// When effective attempts are below SparseThreshold the computed score is linearly
// blended with the baseline so sparse data does not swing the score wildly.
func computeSyncScore(s WindowSummary) float64 {
	w := GetScoreWeights()
	sc := w.Sync
	baseline := syncBaseline(sc)
	sparseThreshold := syncSparseThreshold(sc)

	effective := s.TotalAttempts - s.ExcludedAttempts
	if effective <= 0 {
		return baseline
	}

	successRate := float64(s.SuccessAttempts) / float64(effective)
	base := successRate * sc.BaseWeight

	severityDeduction := computeSeverityDeduction(s, effective, sc.SeverityMax, w.LevelWeights)

	var recoveryBonus float64
	if s.RetryRequests > 0 {
		recoveryBonus = float64(s.RetryRecovered) / float64(s.RetryRequests) * sc.RecoveryWeight
	}

	// Prefer first-token latency; fall back to average duration
	speedMs := s.AvgFirstTokenMs
	if speedMs <= 0 {
		speedMs = s.AvgDurationMs
	}
	speedBonus := tieredScore(speedMs, sc.SpeedWeight, sc.SpeedThresholds, true)

	tpsBonus := tieredScoreHigherBetter(s.AvgOutputTPS, sc.TPSWeight, sc.TPSThresholds)

	score := base - severityDeduction + recoveryBonus + speedBonus + tpsBonus
	score = math.Max(0, math.Min(100, score))

	// Blend with baseline when data is sparse to avoid overly confident scores.
	if effective < sparseThreshold {
		blend := float64(effective) / float64(sparseThreshold)
		score = baseline*(1-blend) + score*blend
	}
	return score
}

// asyncBaseline returns the configured baseline score for async channels,
// defaulting to defaultBaselineScore when the config value is zero (unset).
func asyncBaseline(ac AsyncScoreConfig) float64 {
	if ac.BaselineScore > 0 {
		return ac.BaselineScore
	}
	return defaultBaselineScore
}

// asyncSparseThreshold returns the configured sparse threshold for async
// channels, defaulting to defaultSparseThreshold when unset.
func asyncSparseThreshold(ac AsyncScoreConfig) int64 {
	if ac.SparseThreshold > 0 {
		return ac.SparseThreshold
	}
	return defaultSparseThreshold
}

// computeAsyncScore evaluates: submit success rate, submit speed, exec success rate,
// exec speed (duration), recovery.
//
// Baseline and sparse-blending behaviour mirrors computeSyncScore.
func computeAsyncScore(s WindowSummary) float64 {
	w := GetScoreWeights()
	ac := w.Async
	baseline := asyncBaseline(ac)
	sparseThreshold := asyncSparseThreshold(ac)

	effective := s.TotalAttempts - s.ExcludedAttempts
	if effective <= 0 {
		return baseline
	}

	successRate := float64(s.SuccessAttempts) / float64(effective)
	base := successRate * ac.SubmitBaseWeight

	severityDeduction := computeSeverityDeduction(s, effective, ac.SeverityMax, w.LevelWeights)

	var recoveryBonus float64
	if s.RetryRequests > 0 {
		recoveryBonus = float64(s.RetryRecovered) / float64(s.RetryRequests) * ac.RecoveryWeight
	}

	submitSpeedBonus := tieredScore(s.AvgDurationMs, ac.SubmitSpeedWeight, ac.SubmitSpeedThresholds, true)

	var execSuccessBonus float64
	if s.TaskExecCount > 0 {
		execRate := float64(s.TaskExecSuccess) / float64(s.TaskExecCount)
		execSuccessBonus = execRate * ac.ExecSuccessWeight
	} else {
		execSuccessBonus = ac.ExecSuccessWeight * 0.5
	}

	execSpeedBonus := tieredScore(s.AvgExecDurationMs, ac.ExecSpeedWeight, ac.ExecSpeedThresholds, true)

	score := base - severityDeduction + recoveryBonus + submitSpeedBonus + execSuccessBonus + execSpeedBonus
	score = math.Max(0, math.Min(100, score))

	// Blend with baseline when data is sparse.
	if effective < sparseThreshold {
		blend := float64(effective) / float64(sparseThreshold)
		score = baseline*(1-blend) + score*blend
	}
	return score
}

func computeSeverityDeduction(s WindowSummary, effective int64, severityMax float64, levelWeights [4]float64) float64 {
	var deduction float64
	for lvl := 1; lvl <= 3; lvl++ {
		count := s.ErrorLevelDist[lvl]
		if count > 0 {
			ratio := float64(count) / float64(effective)
			deduction += levelWeights[lvl] * ratio * severityMax
		}
	}
	return math.Min(deduction, severityMax)
}

// tieredScore maps a metric value (lower is better, e.g. latency ms) to a score.
// Returns half weight when no data (value <= 0).
func tieredScore(value, weight float64, th SpeedThreshold, lowerIsBetter bool) float64 {
	if weight <= 0 {
		return 0
	}
	if value <= 0 {
		return weight * 0.5
	}
	if !lowerIsBetter {
		return 0
	}
	switch {
	case value <= th.ExcellentMs:
		return weight
	case value <= th.GoodMs:
		return weight * 0.8
	case value <= th.OkMs:
		return weight * 0.5
	case value <= th.SlowMs:
		return weight * 0.2
	default:
		return 0
	}
}

// tieredScoreHigherBetter maps a metric value (higher is better, e.g. tokens/sec) to a score.
func tieredScoreHigherBetter(value, weight float64, th TPSThreshold) float64 {
	if weight <= 0 {
		return 0
	}
	if value <= 0 {
		return weight * 0.5
	}
	switch {
	case value >= th.Excellent:
		return weight
	case value >= th.Good:
		return weight * 0.8
	case value >= th.Ok:
		return weight * 0.5
	case value >= th.Slow:
		return weight * 0.2
	default:
		return 0
	}
}
