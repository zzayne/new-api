package operation_setting

// StatsErrorExclusionRulesJSON holds the raw JSON for stats error exclusion rules.
// Parsed and applied by service.InitRelayStats via the OnStatsExclusionRulesUpdate callback.
var StatsErrorExclusionRulesJSON = "[]"

// OnStatsExclusionRulesUpdate is a callback set by the service layer to apply
// rule updates without creating a circular dependency (model -> service).
var OnStatsExclusionRulesUpdate func(jsonStr string) error

func StatsErrorExclusionRulesToString() string {
	return StatsErrorExclusionRulesJSON
}

func StatsErrorExclusionRulesFromString(s string) error {
	StatsErrorExclusionRulesJSON = s
	if OnStatsExclusionRulesUpdate != nil {
		return OnStatsExclusionRulesUpdate(s)
	}
	return nil
}
