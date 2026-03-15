package service

// StatsPersistence handles saving and loading stats data for crash recovery.
// User-visible counters are persisted on every window flush (no data loss).
// Admin data (window summaries) is cached periodically (tolerates small loss).
type StatsPersistence interface {
	SaveUserCounters(counters StatsCounters) error
	LoadUserCounters() (StatsCounters, error)
	SaveAdminSnapshot(summaries []WindowSummary) error
	LoadAdminSnapshot() ([]WindowSummary, error)
}

// noopPersistence is the default implementation that does nothing.
// Replace with a DB/Redis backend when persistence is needed.
type noopPersistence struct{}

func (n *noopPersistence) SaveUserCounters(_ StatsCounters) error          { return nil }
func (n *noopPersistence) LoadUserCounters() (StatsCounters, error)        { return StatsCounters{}, nil }
func (n *noopPersistence) SaveAdminSnapshot(_ []WindowSummary) error       { return nil }
func (n *noopPersistence) LoadAdminSnapshot() ([]WindowSummary, error)     { return nil, nil }
