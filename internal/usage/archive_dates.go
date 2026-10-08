package usage

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// SourceRows counts all native attempts in (0, H], including authorized expired
// omissions. Every listed archive row, expired or retained, must match native.
// This report is a scoped comparison, not a deletion or saved-DB trust receipt.
type ArchiveDateVerification struct {
	ArchiveVerification
	RetainFromDate                                string
	RequiredRows, MatchedRows, ExpiredOmittedRows int
}

func validateArchivePartitionDate(entry ManifestFile) error {
	date, err := time.Parse("2006-01-02", entry.Date)
	if err != nil || len(entry.Date) != 10 || date.Year() < 1 || date.Format("2006-01-02") != entry.Date || !strings.HasPrefix(entry.Path, "date="+entry.Date+"/") || filepath.ToSlash(filepath.Clean(filepath.FromSlash(entry.Path))) != entry.Path || strings.Contains(entry.Path, "\\") {
		return fmt.Errorf("%w: invalid archive partition date", ErrDiskUsageCorrupt)
	}
	return nil
}
