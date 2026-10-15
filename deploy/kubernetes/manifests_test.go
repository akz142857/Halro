package kubernetes_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestEveryKubernetesManifestParses(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			payload, err := os.ReadFile(entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			decoder := yaml.NewDecoder(bytes.NewReader(payload))
			for document := 1; ; document++ {
				var value map[string]any
				err := decoder.Decode(&value)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("document %d: %v", document, err)
				}
				if value["apiVersion"] == nil || value["kind"] == nil {
					t.Fatalf("document %d lacks apiVersion or kind", document)
				}
			}
		})
	}
}

func TestBootstrapWorkloadSafetyInvariants(t *testing.T) {
	runtime := manifestText(t, "halro-aws-kms.yaml")
	if !strings.Contains(runtime, `args: ["serve", "--config", "/etc/halro/config.yaml"]`) || strings.Contains(runtime, `args: ["start"`) {
		t.Fatal("runtime manifest must serve prepared storage and never use start")
	}
	if !strings.Contains(runtime, "startupProbe:") {
		t.Fatal("runtime manifest has no startup probe protecting KMS unlock")
	}

	bootstrap := manifestText(t, "halro-bootstrap-job.yaml")
	for _, required := range []string{"completions: 1", "parallelism: 1", "automountServiceAccountToken: false", "AWS_EC2_METADATA_DISABLED"} {
		if !strings.Contains(bootstrap, required) {
			t.Fatalf("bootstrap manifest lacks %q", required)
		}
	}
	initEnd := strings.Index(bootstrap, "      containers:")
	if initEnd < 0 || strings.Contains(bootstrap[:initEnd], "admin-password") {
		t.Fatal("administrator password is mounted by the bootstrap init container")
	}
	if !strings.Contains(bootstrap[initEnd:], "admin-password") {
		t.Fatal("administrator password is not mounted by the bootstrap main container")
	}

	for _, name := range []string{"halro-init-job.yaml", "halro-bootstrap-verify-job.yaml"} {
		text := manifestText(t, name)
		if !strings.Contains(text, "automountServiceAccountToken: false") || !strings.Contains(text, "AWS_EC2_METADATA_DISABLED") {
			t.Fatalf("%s can use an implicit service-account token or EC2 node role", name)
		}
	}
	policy := manifestText(t, "halro-bootstrap-default-deny-network-policy.yaml")
	if !strings.Contains(policy, "policyTypes: [Ingress, Egress]") || !strings.Contains(policy, "egress: []") {
		t.Fatal("bootstrap NetworkPolicy is not default deny")
	}
}

func manifestText(t *testing.T, name string) string {
	t.Helper()
	payload, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}
