package usage

import "time"

// RetentionCutoff is the oldest partition date a prune of the archive may keep.
//
// Partitions are dated in UTC while a retention promise is read in the
// operator's own day. An instance east of UTC reaches its local "N days ago"
// while the UTC partition for that day is still current, so pruning at exactly
// N would delete a day the operator was told they still had. retention_days is
// therefore a floor — at least N days — bought with one extra partition of
// storage.
//
// It lives here rather than at either call site because there are two: the
// maintenance tick and the offline `usage prune` command. They have to agree on
// what "kept for N days" means, and when the rule was written out twice they
// agreed only by inspection.
func RetentionCutoff(now time.Time, retentionDays int) time.Time {
	return now.UTC().AddDate(0, 0, -(retentionDays + 1))
}
