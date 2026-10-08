package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"gopkg.in/yaml.v3"
)

func TestSelectedLegacyExportCommandRejectsUnqualifiedInput(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.Storage.DataDir = filepath.Join(root, "never-created-source")
	cfg.Storage.MasterKey.File = filepath.Join(root, "never-created.key")
	path := filepath.Join(root, "source.yaml")
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "never-created-output")
	for _, args := range [][]string{nil, {"--output", output, "extra"}, {"--unknown"}, {"--config", path, "--output", output}} {
		var report bytes.Buffer
		if err := runSelectedLegacyExportCommand(args, &report); err == nil {
			t.Fatal("unqualified export accepted", args)
		}
		if report.Len() != 0 {
			t.Fatal("failed export reported a verified reader", report.String())
		}
	}
	var report bytes.Buffer
	if err := runMetadataCommand([]string{"export-legacy", "--config", path, "--output", output}, &report); err == nil {
		t.Fatal("metadata route bypassed selected export qualification")
	}
	for _, candidate := range []string{cfg.Storage.DataDir, cfg.Storage.MasterKey.File, output} {
		if _, err := os.Lstat(candidate); !os.IsNotExist(err) {
			t.Fatal("refused CLI changed a path", candidate, err)
		}
	}
}
