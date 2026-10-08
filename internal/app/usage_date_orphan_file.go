package app

import (
	"context"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/usage"
)

type UsageDateOrphanFileReclaimReport struct {
	UsageDateFileReclaimReport
	Entry usage.ManifestFile
}

// One explicit orphan name under the same exclusive offline Standalone authority
// as descriptor-based reclamation. Entry is derived from actual bounded bytes;
// native/source/Metadata/member and actual Close fences govern the whole report.
func ReclaimUsageDateOrphanFile(ctx context.Context, cfg config.Config, retainFrom time.Time, path string) (UsageDateOrphanFileReclaimReport, error) {
	return reclaimUsageDateOrphanFile(ctx, cfg, retainFrom, path, nil, nil)
}

func reclaimUsageDateOrphanFile(ctx context.Context, cfg config.Config, retainFrom time.Time, path string, beforeRevalidate, afterReclaim func() error) (out UsageDateOrphanFileReclaimReport, err error) {
	defer func() {
		if err != nil {
			out = UsageDateOrphanFileReclaimReport{}
		}
	}()
	out.UsageDateFileReclaimReport, err = reclaimUsageDateTarget(ctx, cfg, retainFrom, func(ctx context.Context, p *usage.DiskProjection, e *usage.Exporter) (usage.DateFileReclamation, error) {
		result, err := e.ReclaimRetainedDateOrphanFile(ctx, p, retainFrom, path, usage.DefaultDateFileReclaimLimits())
		out.Entry = result.Entry
		return result.Reclamation, err
	}, beforeRevalidate, afterReclaim)
	return out, err
}
