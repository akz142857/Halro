package app

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestPrimaryUnconfirmedMetricsExposeBoundedRecoveryWithoutIdentityLabels(t *testing.T) {
	var buffer bytes.Buffer
	output := bufio.NewWriter(&buffer)
	stats := resourcebudget.Stats{Used: resourcebudget.Cost{Unconfirmed: 300}}
	stats.Classes[resourcebudget.IO] = resourcebudget.Cost{Unconfirmed: 200}
	stats.Classes[resourcebudget.Recovery] = resourcebudget.Cost{Unconfirmed: 100}
	writePrimaryUnconfirmedMetrics(output, config.ResourceBudget{
		TotalUnconfirmedBytes: 1024, IOUnconfirmedBytes: 768,
		RecoveryUnconfirmedBytes: 512, RecoveryReserveUnconfirmedBytes: 256,
		UnconfirmedMaxEntries: 8,
	}, stats, replication.UnconfirmedAdmissionStatus{
		Entries: 3, RecoveryBytes: 400, RecoveryCharged: 256, OverLimit: true,
		AdmissionRefusals: 2, LastProgress: time.Unix(123, 0),
	})
	if err := output.Flush(); err != nil {
		t.Fatal(err)
	}
	text := buffer.String()
	for _, want := range []string{
		"halro_replication_unconfirmed_bytes{kind=\"configured_total\"} 1024",
		"halro_replication_unconfirmed_bytes{kind=\"used_total\"} 300",
		"halro_replication_unconfirmed_entries{kind=\"used\"} 3",
		"halro_replication_unconfirmed_recovery_debt_bytes 144",
		"halro_replication_unconfirmed_recovery_over_limit 1",
		"halro_replication_unconfirmed_admission_refusals_total 2",
		"halro_replication_unconfirmed_last_progress_timestamp_seconds 123.000",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("unconfirmed metric %q is missing from:\n%s", want, text)
		}
	}
	if strings.Contains(text, "peer=") || strings.Contains(text, "request_id=") {
		t.Fatalf("unconfirmed metrics contain high-cardinality identity labels:\n%s", text)
	}
}
