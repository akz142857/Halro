package observability_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestEveryAlertHasAResolvableRunbook(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("prometheus/alert-rules.yml")
	if err != nil {
		t.Fatal(err)
	}
	var ruleFile struct {
		Groups []struct {
			Rules []struct {
				Alert       string            `yaml:"alert"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(data, &ruleFile); err != nil {
		t.Fatalf("decode alert rules: %v", err)
	}

	for _, group := range ruleFile.Groups {
		for _, rule := range group.Rules {
			if rule.Alert == "" {
				continue
			}
			runbookURL := rule.Annotations["runbook_url"]
			if runbookURL == "" {
				t.Errorf("alert %s has no runbook_url", rule.Alert)
				continue
			}
			path, anchor, _ := strings.Cut(runbookURL, "#")
			if !strings.HasPrefix(path, "/docs/") {
				t.Errorf("alert %s has non-repository runbook_url %q", rule.Alert, runbookURL)
				continue
			}
			contents, err := os.ReadFile(filepath.Join("../..", strings.TrimPrefix(path, "/")))
			if err != nil {
				t.Errorf("alert %s runbook %q is not readable: %v", rule.Alert, path, err)
				continue
			}
			if path != "/docs/observability/operations-runbook.md" {
				continue
			}
			wantAnchor := strings.ToLower(rule.Alert)
			if anchor != wantAnchor {
				t.Errorf("alert %s runbook_url anchor = %q, want %q", rule.Alert, anchor, wantAnchor)
				continue
			}
			if !strings.Contains(string(contents), "### "+rule.Alert+"\n") {
				t.Errorf("alert %s runbook heading is missing", rule.Alert)
			}
		}
	}
}
