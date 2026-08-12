package app

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

var (
	samplePattern = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{([^}]*)\})?[[:space:]]`)
	labelPattern  = regexp.MustCompile(`(?:^|,)([a-zA-Z_][a-zA-Z0-9_]*)=`)
	// Two shapes emit a family name: the metricHeader calls, and the helpers
	// that take the name as their first literal argument. Matching only the
	// first left every histogram family outside the inventory, which is the same
	// structural blindness this static pass exists to remove.
	staticMetricPattern = regexp.MustCompile(`(?:metricHeader|writeLatencyHistogram)\(output,\s*(?:\n\s*)?"([^"]+)"`)
)

func assertMetricsExpositionContract(t *testing.T, body string) {
	t.Helper()
	help := make(map[string]int)
	types := make(map[string]int)
	families := make(map[string]struct{})
	allowedLabels := map[string]struct{}{
		"le": {}, "status": {}, "direction": {}, "reason": {}, "provider_type": {},
		"provider_id": {}, "deployment_id": {}, "version": {}, "commit": {},
		"operation": {}, "error_class": {}, "purpose": {}, "state": {}, "capability": {},
		"target_kind": {},
		// Capability profile IDs are a fixed set compiled into the binary.
		"profile": {},
		// Constant for the life of a process; they identify the node's time
		// zone rules so a fleet can be checked for agreement.
		"source": {}, "fingerprint": {},
	}
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "# HELP ") {
			fields := strings.Fields(line)
			if len(fields) < 4 {
				t.Fatalf("invalid HELP line %q", line)
			}
			help[fields[2]]++
			continue
		}
		if strings.HasPrefix(line, "# TYPE ") {
			fields := strings.Fields(line)
			if len(fields) != 4 {
				t.Fatalf("invalid TYPE line %q", line)
			}
			types[fields[2]]++
			continue
		}
		match := samplePattern.FindStringSubmatch(line)
		if match == nil {
			t.Fatalf("invalid sample line %q", line)
		}
		family := metricFamily(match[1], types)
		families[family] = struct{}{}
		for _, label := range labelPattern.FindAllStringSubmatch(match[2], -1) {
			if _, ok := allowedLabels[label[1]]; !ok {
				t.Fatalf("metric %s has undeclared label %q", match[1], label[1])
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	for family := range families {
		if help[family] != 1 || types[family] != 1 {
			t.Fatalf("metric family %s has HELP=%d TYPE=%d", family, help[family], types[family])
		}
	}
	for family, count := range help {
		if count != 1 || types[family] != 1 {
			t.Fatalf("declared metric family %s has HELP=%d TYPE=%d", family, count, types[family])
		}
	}

	reference, err := os.ReadFile("../../docs/contracts/metrics-reference.md")
	if err != nil {
		t.Fatal(err)
	}
	for family := range families {
		if !strings.HasPrefix(family, "halro_") {
			continue
		}
		if !strings.Contains(string(reference), fmt.Sprintf("`%s`", family)) {
			t.Fatalf("exported metric %s is missing from docs/contracts/metrics-reference.md", family)
		}
	}

	// Runtime exposition can omit conditional families, such as audit-anchor
	// metrics when anchoring is disabled. Inventory literal metricHeader calls
	// as well so adding a conditional exporter cannot silently bypass the docs
	// contract merely because the test fixture does not activate its branch.
	metricsSource, err := os.ReadFile("metrics.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, match := range staticMetricPattern.FindAllSubmatch(metricsSource, -1) {
		family := string(match[1])
		if !strings.HasPrefix(family, "halro_") {
			continue
		}
		if !strings.Contains(string(reference), fmt.Sprintf("`%s`", family)) {
			t.Fatalf("statically exported metric %s is missing from docs/contracts/metrics-reference.md", family)
		}
	}
}

func metricFamily(sample string, types map[string]int) string {
	if types[sample] == 1 {
		return sample
	}
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		candidate := strings.TrimSuffix(sample, suffix)
		if candidate != sample && types[candidate] == 1 {
			return candidate
		}
	}
	return sample
}
