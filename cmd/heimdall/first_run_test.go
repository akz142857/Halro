package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Heimdall/internal/config"
)

// Config validation reports every problem it found in one error. Routing that
// through the JSON handler flattened all of them into a single line of escaped
// newlines, which threw away the one thing the validator does unusually well.
func TestCommandFailureListsEveryProblemOnItsOwnLine(t *testing.T) {
	var out bytes.Buffer
	reportCommandFailure(&out, errors.New(strings.Join([]string{
		"storage.data_dir is required",
		"server.gateway_listen is required",
		"usage.timezone is required",
	}, "\n")))

	rendered := out.String()
	if strings.Contains(rendered, `\n`) {
		t.Fatalf("problems were escaped onto one line: %s", rendered)
	}
	if !strings.Contains(rendered, "3 problems found") {
		t.Fatalf("output does not count the problems: %s", rendered)
	}
	for _, problem := range []string{
		"storage.data_dir is required",
		"server.gateway_listen is required",
		"usage.timezone is required",
	} {
		if !strings.Contains(rendered, "  - "+problem) {
			t.Fatalf("problem %q is not listed on its own line: %s", problem, rendered)
		}
	}
}

func TestSingleProblemIsReportedWithoutACount(t *testing.T) {
	var out bytes.Buffer
	reportCommandFailure(&out, errors.New("open config: no such file or directory"))
	rendered := strings.TrimSpace(out.String())
	if rendered != "heimdall: open config: no such file or directory" {
		t.Fatalf("unexpected rendering: %q", rendered)
	}
}

// Redaction came free while this went through the logger. An error can quote a
// value out of the configuration it was reading, so it must not be lost by
// printing the error directly.
func TestCommandFailureStillRedacts(t *testing.T) {
	var out bytes.Buffer
	reportCommandFailure(&out, errors.New("provider rejected sk-live-abcdefghijklmnop"))
	if strings.Contains(out.String(), "sk-live-abcdefghijklmnop") {
		t.Fatalf("credential survived into the terminal output: %s", out.String())
	}
}

// The key is created during the first run, is deliberately absent from the
// encrypted backups, and cannot be reconstructed. Saying so belongs where the
// risk starts rather than in a document the operator has no reason to open yet.
func TestFirstRunNamesTheMasterKeyAndWhatLosingItCosts(t *testing.T) {
	var out bytes.Buffer
	cfg := config.Config{}
	cfg.Storage.MasterKey.Mode = config.MasterKeyModeFile
	cfg.Storage.MasterKey.File = "master.key"
	printMasterKeyCustodyNotice(&out, cfg)

	notice := out.String()
	absolute, err := filepath.Abs("master.key")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(notice, absolute) {
		t.Fatalf("notice does not give the absolute location: %s", notice)
	}
	for _, expected := range []string{"backups do NOT", "cannot be recovered", "backup-restore.md"} {
		if !strings.Contains(notice, expected) {
			t.Fatalf("notice omits %q: %s", expected, notice)
		}
	}
}

// Under KMS custody there is no local key file, so the warning would be a lie.
func TestNoCustodyNoticeWhenTheKeyLivesInKMS(t *testing.T) {
	var out bytes.Buffer
	cfg := config.Config{}
	cfg.Storage.MasterKey.Mode = config.MasterKeyModeKeySlots
	printMasterKeyCustodyNotice(&out, cfg)
	if out.Len() != 0 {
		t.Fatalf("printed a local-file warning under KMS custody: %s", out.String())
	}
}
