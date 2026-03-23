package service

import "time"

// RelayIdentity carries the relay context fields needed by the stats tracker.
// Defined in service to avoid importing relay/common (which would cause a cycle).
type RelayIdentity struct {
	UserID    int
	TokenID   int
	Group     string
	ModelName string
	RelayMode int
}

// relayStatsTracker encapsulates per-request stats tracking state used
// inside retry loops (Relay, RelayTask, RelayMidjourney). It handles
// attempt event collection, excluded/real error counting, and final
// request-complete emission.
type relayStatsTracker struct {
	collector        RelayStatsCollector
	identity         RelayIdentity
	requestID        string
	requestStart     time.Time
	isAsync          bool
	channelChain     []int
	firstErrCode     string
	firstErrStatus   int
	excludedAttempts int
	realErrAttempts  int
	totalAttempts    int
	hadError         bool
}

func NewRelayStatsTracker(requestID string, identity RelayIdentity, isAsync bool) *relayStatsTracker {
	return &relayStatsTracker{
		collector:    GetRelayStatsCollector(),
		identity:     identity,
		requestID:    requestID,
		requestStart: time.Now(),
		isAsync:      isAsync,
	}
}

// TrackAttempt collects an attempt event, updates internal counters.
// The event's Excluded and ErrorLevel fields are populated by the collector
// (via the ErrorClassifier) since the event is passed by pointer.
func (t *relayStatsTracker) TrackAttempt(evt *AttemptEvent) {
	evt.IsAsync = t.isAsync
	t.totalAttempts++
	t.channelChain = append(t.channelChain, evt.ChannelID)

	SafeCollectAttempt(t.collector, evt)

	if !evt.Success {
		t.hadError = true
		if evt.Excluded {
			t.excludedAttempts++
		} else {
			t.realErrAttempts++
		}
		if t.firstErrCode == "" {
			t.firstErrCode = evt.ErrorCode
			t.firstErrStatus = evt.StatusCode
		}
	}
}

// Complete emits the final RequestCompleteEvent for this request.
func (t *relayStatsTracker) Complete(finalSuccess bool) {
	SafeCollectRequestComplete(t.collector, RequestCompleteEvent{
		RequestID:            t.requestID,
		UserID:               t.identity.UserID,
		TokenID:              t.identity.TokenID,
		Group:                t.identity.Group,
		OriginalModel:        t.identity.ModelName,
		RelayMode:            t.identity.RelayMode,
		IsAsync:              t.isAsync,
		TotalAttempts:        t.totalAttempts,
		FinalSuccess:         finalSuccess,
		HasRetry:             t.totalAttempts > 1,
		RetryRecovered:       t.hadError && finalSuccess,
		ChannelChain:         t.channelChain,
		TotalDuration:        time.Since(t.requestStart),
		FirstErrorCode:       t.firstErrCode,
		FirstErrorStatusCode: t.firstErrStatus,
		ExcludedAttempts:     t.excludedAttempts,
		RealErrorAttempts:    t.realErrAttempts,
	})
}
