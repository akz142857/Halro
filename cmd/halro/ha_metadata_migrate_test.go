package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

func TestHAMetadataCommandRequiresIndependentKeysAndExplicitMaintenance(t *testing.T) {
	for _, args := range [][]string{{"ha-prepare"}, {"ha-maintain"}, {"ha-install"}, {"ha-checkpoint"}, {"ha-maintain", "--activate-runtime"}, {"ha-prepare", "--resume-activation"}, {"ha-install", "--from-directory", "/candidate-header-alone"}} {
		var output bytes.Buffer
		if err := runMetadataCommand(args, &output); err == nil || output.Len() != 0 {
			t.Fatal("unqualified ordinary/partial flags authorized HA maintenance", args, err, output.String())
		}
	}
}

func TestHAMetadataCommandRejectsActualStandaloneBeforePublishingOrLeakingKeys(t *testing.T) {
	cfg, a, manager, metadata, journal := metadataMigrationCommandFixture(t)
	beforeMetadata, err := os.ReadFile(metadata)
	if err != nil {
		t.Fatal(err)
	}
	beforeJournal, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"ha-maintain", "ha-install", "ha-checkpoint"} {
		before := manager.Snapshot().Used
		request := haMetadataCommandArguments{command: command, keys: [5]string{a.ledgerKey, a.ledgerKey, a.ledgerKey, a.ledgerKey, a.sourceJournalKey}, sourceDirectory: a.output}
		var output bytes.Buffer
		err := executeHAMetadataCommand(context.Background(), cfg, request, manager, &output)
		if err == nil || output.Len() != 0 {
			t.Fatal("Standalone acquired maintenance role", command, err, output.String())
		}
		if strings.Contains(err.Error(), string(bytes.Repeat([]byte{73}, 32))) {
			t.Fatal("key contents leaked in error")
		}
		if manager.Snapshot().Used != before {
			t.Fatal("actual rejection leaked key/guard work fees", manager.Snapshot(), before)
		}
	}
	afterMetadata, err := os.ReadFile(metadata)
	if err != nil {
		t.Fatal(err)
	}
	afterJournal, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeMetadata, afterMetadata) || !bytes.Equal(beforeJournal, afterJournal) {
		t.Fatal("refused HA command changed Standalone source")
	}
}
