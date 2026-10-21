package kubernetes_test

import (
	"bytes"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
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

func TestHAWorkloadSafetyInvariants(t *testing.T) {
	ha := manifestText(t, "halro-ha-statefulset.yaml")
	for _, required := range []string{
		"publishNotReadyAddresses: true",
		"replicas: 3",
		"podManagementPolicy: Parallel",
		"updateStrategy: {type: OnDelete}",
		"persistentVolumeClaimRetentionPolicy: {whenDeleted: Retain, whenScaled: Retain}",
		"accessModes: [ReadWriteOncePod]",
		"minAvailable: 2",
		"automountServiceAccountToken: false",
		"port: 9910",
		"{name: metrics, port: 9090, targetPort: metrics}",
	} {
		if !strings.Contains(ha, required) {
			t.Fatalf("HA manifest lacks %q", required)
		}
	}
	if strings.Contains(ha, "pods/patch") || strings.Contains(ha, "halro.io/role=primary") {
		t.Fatal("HA manifest must use client routing option (a), not Kubernetes role mutation")
	}
}

func TestHAObservabilityIngressPolicyIsNarrow(t *testing.T) {
	var policy struct {
		Spec struct {
			PodSelector struct {
				MatchLabels map[string]string `yaml:"matchLabels"`
			} `yaml:"podSelector"`
			PolicyTypes []string `yaml:"policyTypes"`
			Ingress     []struct {
				From []struct {
					NamespaceSelector *struct {
						MatchLabels map[string]string `yaml:"matchLabels"`
					} `yaml:"namespaceSelector"`
					PodSelector *struct {
						MatchLabels map[string]string `yaml:"matchLabels"`
					} `yaml:"podSelector"`
					IPBlock any `yaml:"ipBlock"`
				} `yaml:"from"`
				Ports []struct {
					Protocol string `yaml:"protocol"`
					Port     int    `yaml:"port"`
				} `yaml:"ports"`
			} `yaml:"ingress"`
			Egress []any `yaml:"egress"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(manifestText(t, "halro-ha-observability-ingress.example.yaml")), &policy); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(policy.Spec.PodSelector.MatchLabels, map[string]string{
		"app.kubernetes.io/name": "halro", "app.kubernetes.io/component": "ha-member",
	}) || !slices.Equal(policy.Spec.PolicyTypes, []string{"Ingress"}) || len(policy.Spec.Egress) != 0 ||
		len(policy.Spec.Ingress) != 2 {
		t.Fatal("HA observability policy must select only HA members with two reviewed ingress routes")
	}
	wantByPort := map[int]map[string]bool{
		9090: {"prometheus": true, "halro-ha-health": true},
		8080: {"halro-ha-health": true},
	}
	for _, ingress := range policy.Spec.Ingress {
		if len(ingress.Ports) != 1 || ingress.Ports[0].Protocol != "TCP" {
			t.Fatal("HA observability policy has a broad or non-TCP port")
		}
		port := ingress.Ports[0].Port
		wantPods, ok := wantByPort[port]
		if !ok || len(ingress.From) != len(wantPods) {
			t.Fatal("HA observability policy has an unexpected or repeated ingress port")
		}
		for _, from := range ingress.From {
			if from.NamespaceSelector == nil || from.PodSelector == nil || from.IPBlock != nil ||
				!maps.Equal(from.NamespaceSelector.MatchLabels, map[string]string{"halro.io/monitoring-access": "allowed"}) ||
				len(from.PodSelector.MatchLabels) != 1 || !wantPods[from.PodSelector.MatchLabels["app.kubernetes.io/name"]] {
				t.Fatal("HA observability policy has a broad or unexpected source selector")
			}
			delete(wantPods, from.PodSelector.MatchLabels["app.kubernetes.io/name"])
		}
		if len(wantPods) != 0 {
			t.Fatal("HA observability policy repeated a source instead of covering the expected workloads")
		}
		delete(wantByPort, port)
	}
	if len(wantByPort) != 0 {
		t.Fatal("HA observability policy is missing an expected ingress route")
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
