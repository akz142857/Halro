package usage

func hotScratchBucket(name string) bool {
	switch name {
	case "meta", "hourly", "hourly_order", "request_quality", "dashboard_period", "dashboard_group", "dashboard_top", "rollup", "rollup_counts", "rollup_scope", "rollup_range":
		return true
	}
	return false
}
