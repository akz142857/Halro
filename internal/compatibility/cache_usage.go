package compatibility

import "log/slog"

// NormalizeCacheWriteLong reconciles a reported lifetime split with the
// authoritative write total. Diagnostics include only token counts.
func NormalizeCacheWriteLong(total, short, long int64) int64 {
	if short < 0 || long < 0 || total < 0 || short > total || long != total-short {
		slog.Warn("provider cache write breakdown does not match total",
			"total_tokens", total, "short_tokens", short, "long_tokens", long)
	}
	return min(max(0, long), max(0, total))
}
