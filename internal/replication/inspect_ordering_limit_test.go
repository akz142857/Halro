package replication

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestOrderingInspectionFileCeilingRefusesWholeSourceWithoutRepair(t *testing.T) {
	path, key, header, records := orderingInspectionFixture(t)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	report, err := InspectOrderingJournalWithLimit(context.Background(), path, key, header.ClusterID, header.Incarnation, 6, records[5].MAC, 1, info.Size()-1)
	if !errors.Is(err, ErrOrderingJournalDiskLimit) || report != (OrderingInspection{}) {
		t.Fatal("oversized source decoded", report, err)
	}
	if report, err := InspectOrderingJournalWithLimit(context.Background(), path, key, header.ClusterID, header.Incarnation, 6, records[5].MAC, 1, info.Size()); err != nil || report.Head.Index != uint64(len(records)) || report.Durable.Index != 6 || report.Allowed.Index != 1 {
		t.Fatal(report, err)
	}
	if after, err := os.Stat(path); err != nil || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		t.Fatal("inspection changed source", after, err)
	}
}
