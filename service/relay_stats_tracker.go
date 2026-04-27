package service

import (
	"time"

	"github.com/QuantumNous/new-api/setting/operation_setting"
)

// RelayIdentity carries the relay context fields needed by the stats tracker.
// Defined in service to avoid importing relay/common (which would cause a cycle).
type RelayIdentity struct {
	UserID    int
	TokenID   int
	Group     string
	ModelName string
	RelayMode int
}

// groupOutcome tracks per-group attempt outcomes within a single request.
// When auto group retries span multiple groups, each group gets its own outcome.
type groupOutcome struct {
	group          string
	channelChain   []int
	attempts       int
	excluded       int
	realErrors     int
	firstErrCode   string
	firstErrStatus int
	hadError       bool
}

// relayStatsTracker encapsulates per-request stats tracking state used
// inside retry loops (Relay, RelayTask, RelayMidjourney). It handles
// attempt event collection, excluded/real error counting, and final
// per-group request-complete emission.
type relayStatsTracker struct {
	collector        RelayStatsCollector
	identity         RelayIdentity
	requestID        string
	requestStart     time.Time
	isAsync          bool
	totalAttempts    int

	currentGroup     *groupOutcome
	groupOutcomes    []*groupOutcome
}

// NewRelayStatsTracker returns nil when stats are disabled, avoiding all
// per-request allocations. All methods are nil-receiver safe.
func NewRelayStatsTracker(requestID string, identity RelayIdentity, isAsync bool) *relayStatsTracker {
	if !operation_setting.IsRelayStatsEnabled() {
		return nil
	}
	return &relayStatsTracker{
		collector:    GetRelayStatsCollector(),
		identity:     identity,
		requestID:    requestID,
		requestStart: time.Now(),
		isAsync:      isAsync,
	}
}

func (t *relayStatsTracker) getOrCreateGroup(group string) *groupOutcome {
	if t.currentGroup != nil && t.currentGroup.group == group {
		return t.currentGroup
	}
	g := &groupOutcome{group: group}
	t.groupOutcomes = append(t.groupOutcomes, g)
	t.currentGroup = g
	return g
}

// TrackAttempt collects an attempt event, updates internal counters.
// The event's Excluded and ErrorLevel fields are populated by the collector
// (via the ErrorClassifier) since the event is passed by pointer.
// Nil-receiver safe: no-op when stats are disabled.
func (t *relayStatsTracker) TrackAttempt(evt *AttemptEvent) {
	if t == nil {
		return
	}
	evt.IsAsync = t.isAsync
	t.totalAttempts++

	g := t.getOrCreateGroup(evt.Group)
	g.attempts++
	g.channelChain = append(g.channelChain, evt.ChannelID)

	SafeCollectAttempt(t.collector, evt)

	if !evt.Success {
		g.hadError = true
		if evt.Excluded {
			g.excluded++
		} else {
			g.realErrors++
		}
		if g.firstErrCode == "" {
			g.firstErrCode = evt.ErrorCode
			g.firstErrStatus = evt.StatusCode
		}
	}
}

// Complete emits per-group RequestCompleteEvents.
// Groups tried before the final group are recorded as failures.
// The last group gets the overall finalSuccess outcome.
// Nil-receiver safe: no-op when stats are disabled.
func (t *relayStatsTracker) Complete(finalSuccess bool) {
	if t == nil {
		return
	}
	for i, g := range t.groupOutcomes {
		isLastGroup := i == len(t.groupOutcomes)-1
		groupSuccess := isLastGroup && finalSuccess

		SafeCollectRequestComplete(t.collector, RequestCompleteEvent{
			RequestID:            t.requestID,
			UserID:               t.identity.UserID,
			TokenID:              t.identity.TokenID,
			Group:                g.group,
			OriginalModel:        t.identity.ModelName,
			RelayMode:            t.identity.RelayMode,
			IsAsync:              t.isAsync,
			TotalAttempts:        g.attempts,
			FinalSuccess:         groupSuccess,
			HasRetry:             g.attempts > 1 || len(t.groupOutcomes) > 1,
			RetryRecovered:       g.hadError && groupSuccess,
			ChannelChain:         g.channelChain,
			TotalDuration:        time.Since(t.requestStart),
			FirstErrorCode:       g.firstErrCode,
			FirstErrorStatusCode: g.firstErrStatus,
			ExcludedAttempts:     g.excluded,
			RealErrorAttempts:    g.realErrors,
		})
	}
}
