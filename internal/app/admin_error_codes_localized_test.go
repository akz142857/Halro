package app

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every refusal the Admin API states in English has to be sayable in the
// operator's language, and the code is the only part of the answer the console
// can translate. Before the codes existed the console printed the server's
// English sentence under a translated headline, to every reader in every
// language; a refusal added here without a mapping would quietly do that again.
//
// The scan reads the call itself rather than the message, because the message
// is the server's own wording and is meant to keep changing. A refusal with no
// mapping is a finding, not a build break for the console: it falls back to the
// generic headline plus the English reason, which is what all of these did
// before. The test is what makes that fallback a decision instead of an
// oversight.
func TestAdminRefusalCodesAreLocalized(t *testing.T) {
	codes := adminErrorCodesInSource(t)
	if len(codes) == 0 {
		t.Fatal("found no coded admin refusals; the scan below stopped matching the call sites")
	}
	mapping, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "i18n", "errors.ts"))
	if err != nil {
		t.Fatalf("read the console's error mapping: %v", err)
	}
	var missing []string
	for _, code := range codes {
		if !strings.Contains(string(mapping), code+":") {
			missing = append(missing, code)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("admin refusals the console cannot translate, so every reader gets the English sentence: %v", missing)
	}
}

var adminCodedRefusal = regexp.MustCompile(`adminBadRequestCode\(\s*\w+,\s*"([a-z0-9_]+)"|adminBadRequestFields\(\s*\w+,\s*"([a-z0-9_]+)"|"code":\s*"([a-z0-9_]+)"|codedErrorBody\(\s*"([a-z0-9_]+)"`)

func adminErrorCodesInSource(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		// Admin handlers only. The gateway's own refusals travel to SDK clients
		// in the provider wire format, where English is the contract rather than
		// a reader's language, and they never reach the console.
		if entry.IsDir() || !strings.HasPrefix(name, "admin_") ||
			!strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, match := range adminCodedRefusal.FindAllStringSubmatch(string(source), -1) {
			for _, group := range match[1:] {
				if group != "" {
					seen[group] = true
				}
			}
		}
	}
	codes := make([]string, 0, len(seen))
	for code := range seen {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}
