package app

import (
	"encoding/json"
	"errors"
	"unicode/utf8"
)

const usageSummaryResponseBytes = 8 << 20

type usageSummaryResponse struct {
	Granularity       string                  `json:"granularity"`
	Start             string                  `json:"start"`
	End               string                  `json:"end"`
	Buckets           []summaryBucket         `json:"buckets"`
	Totals            summaryMetrics          `json:"totals"`
	TimezoneChanges   []summaryTimezoneChange `json:"timezone_changes"`
	WatermarkSequence uint64                  `json:"watermark_sequence"`
	TimeContext       timeContext             `json:"time_context"`
	GroupBy           string                  `json:"group_by,omitempty"`
	Groups            *[]summaryGroup         `json:"groups,omitempty"`
	GroupsTruncated   *bool                   `json:"groups_truncated,omitempty"`
	GroupsOtherCount  *int                    `json:"groups_other_count,omitempty"`
	Sort              string                  `json:"sort,omitempty"`
	Order             string                  `json:"order,omitempty"`
	Filter            *map[string]string      `json:"filter,omitempty"`
	ResourceLabels    *map[string]string      `json:"resource_labels,omitempty"`
}

var errUsageSummaryResponseLimit = errors.New("usage summary response exceeds limits")

// Preflight escaped strings without allocating encoded copies. All remaining
// numeric/framing work has a fixed bound from the structure-count ceilings.
// Final Marshal still checks exact total bytes before any success header/write.
func marshalUsageSummaryResponse(body usageSummaryResponse) ([]byte, error) {
	ceiling, known := summaryBucketCeilings[body.Granularity]
	if !known || len(body.Buckets) > ceiling || len(body.TimezoneChanges) > 4096 || body.Groups != nil && len(*body.Groups) > 101 || body.ResourceLabels != nil && len(*body.ResourceLabels) > 101 || body.Filter != nil && len(*body.Filter) > 2 {
		return nil, errUsageSummaryResponseLimit
	}
	remaining := usageSummaryResponseBytes
	charge := func(value string) error {
		if len(value) > remaining {
			return errUsageSummaryResponseLimit
		}
		n := 2
		for i := 0; i < len(value); {
			c := value[i]
			size, cost := 1, 1
			switch {
			case c == '"' || c == '\\' || c == '\n' || c == '\r' || c == '\t' || c == '\b' || c == '\f':
				cost = 2
			case c < 0x20 || c == '<' || c == '>' || c == '&':
				cost = 6
			case c >= utf8.RuneSelf:
				r, w := utf8.DecodeRuneInString(value[i:])
				size, cost = w, w
				if r == utf8.RuneError && w == 1 || r == '\u2028' || r == '\u2029' {
					cost = 6
				}
			}
			if cost > remaining-n {
				return errUsageSummaryResponseLimit
			}
			n += cost
			i += size
		}
		if n > remaining {
			return errUsageSummaryResponseLimit
		}
		remaining -= n
		return nil
	}
	for _, s := range []string{body.Granularity, body.Start, body.End, body.GroupBy, body.Sort, body.Order, body.TimeContext.AccountingTimezone, body.TimeContext.PeriodID, body.TimeContext.PendingTimezone} {
		if err := charge(s); err != nil {
			return nil, err
		}
	}
	for _, bucket := range body.Buckets {
		if err := charge(bucket.Period); err != nil {
			return nil, err
		}
	}
	for _, change := range body.TimezoneChanges {
		if err := charge(change.PeriodID); err != nil {
			return nil, err
		}
	}
	if body.Groups != nil {
		for _, group := range *body.Groups {
			if err := charge(group.Key); err != nil {
				return nil, err
			}
		}
	}
	for _, m := range []*map[string]string{body.Filter, body.ResourceLabels} {
		if m != nil {
			for key, value := range *m {
				if err := charge(key); err != nil {
					return nil, err
				}
				if err := charge(value); err != nil {
					return nil, err
				}
			}
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	if len(raw)+1 > usageSummaryResponseBytes {
		return nil, errUsageSummaryResponseLimit
	}
	return append(raw, '\n'), nil
}
