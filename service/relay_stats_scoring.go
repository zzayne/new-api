package service

import "math"

// Scoring weights (can be extracted to setting later)
//
// A perfectly healthy channel (100% success, fast responses) should score ~95.
// Recovery bonus rewards resilience, pushing up to 100 for channels that
// recover well from transient errors.
const (
	scoreBaseWeight     = 75.0 // max points from success rate
	scoreSeverityMax    = 25.0 // max deduction from error severity
	scoreRecoveryWeight = 5.0  // max points from retry recovery (bonus for resilience)
	scoreSpeedWeight    = 20.0 // max points from response speed

	// Response time thresholds (ms) for speed scoring
	speedExcellentMs = 500
	speedGoodMs      = 2000
	speedOkMs        = 5000
	speedSlowMs      = 10000
)

// Error level weights for severity deduction
var levelWeights = [4]float64{
	0, // level 0: excluded, no deduction
	1, // level 1: normal error
	3, // level 2: serious
	6, // level 3: critical
}

// ComputeChannelScore calculates a 0-100 health score for a window summary.
func ComputeChannelScore(s WindowSummary) float64 {
	effective := s.TotalAttempts - s.ExcludedAttempts
	if effective <= 0 {
		return 100
	}

	// Base score: success rate × 60
	successRate := float64(s.SuccessAttempts) / float64(effective)
	base := successRate * scoreBaseWeight

	// Severity deduction: weighted sum of error levels
	var severityDeduction float64
	for lvl := 1; lvl <= 3; lvl++ {
		count := s.ErrorLevelDist[lvl]
		if count > 0 {
			ratio := float64(count) / float64(effective)
			severityDeduction += levelWeights[lvl] * ratio * scoreSeverityMax
		}
	}
	severityDeduction = math.Min(severityDeduction, scoreSeverityMax)

	// Recovery bonus: recovery rate × 10
	var recoveryBonus float64
	if s.RetryRequests > 0 {
		recoveryBonus = float64(s.RetryRecovered) / float64(s.RetryRequests) * scoreRecoveryWeight
	}

	// Speed bonus: based on average response time
	speedBonus := computeSpeedScore(s.AvgDurationMs)

	score := base - severityDeduction + recoveryBonus + speedBonus
	return math.Max(0, math.Min(100, score))
}

func computeSpeedScore(avgMs float64) float64 {
	if avgMs <= 0 {
		return scoreSpeedWeight
	}
	switch {
	case avgMs <= speedExcellentMs:
		return scoreSpeedWeight
	case avgMs <= speedGoodMs:
		return scoreSpeedWeight * 0.8
	case avgMs <= speedOkMs:
		return scoreSpeedWeight * 0.5
	case avgMs <= speedSlowMs:
		return scoreSpeedWeight * 0.2
	default:
		return 0
	}
}
