package app

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	referenceconfigs "github.com/akz142857/Halro/configs"
	"github.com/akz142857/Halro/internal/config"
)

// Every configuration key has to be written down somewhere an operator reads.
//
// Two gates already compare these files against Default(), and both compare
// *decoded values*. That leaves one blind spot, and it is the one that matters:
// a key whose default is the zero value can be missing from an annotated file
// entirely and still decode to exactly what Default() holds. An absent section
// and a section set to its zero value are indistinguishable after decoding.
//
// It had already happened twice. `provider_subscriptions` never reached the
// first-run template, and the whole `key_slots` half of `storage.master_key` —
// five keys plus an entry shape of six more — was in neither file, so an
// operator moving to a KMS-held Master Key had no reference for any of it while
// `configs/embed.go` called the file "the complete annotated configuration
// reference".
//
// So this gate compares *keys*, not values, and reads the struct rather than
// either file: reflection over config.Config is the only enumeration that
// cannot itself drift.

// configKeysFromStruct walks the configuration type and returns every yaml key
// name it can produce, including the omitempty ones that never appear in a
// marshalled Default() and are therefore invisible to the value gates.
func configKeysFromStruct(t *testing.T) []string {
	t.Helper()
	seen := map[string]struct{}{}
	var walk func(reflect.Type)
	walk = func(structType reflect.Type) {
		for index := range structType.NumField() {
			field := structType.Field(index)
			tag := field.Tag.Get("yaml")
			if tag == "" || tag == "-" {
				continue
			}
			name, _, _ := strings.Cut(tag, ",")
			if name == "" {
				continue
			}
			seen[name] = struct{}{}
			fieldType := field.Type
			for fieldType.Kind() == reflect.Pointer || fieldType.Kind() == reflect.Slice {
				fieldType = fieldType.Elem()
			}
			// Duration and the other named scalars are structs to nobody.
			if fieldType.Kind() == reflect.Struct && fieldType.PkgPath() != "time" {
				walk(fieldType)
			}
		}
	}
	walk(reflect.TypeOf(config.Config{}))
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// documentedKeys returns the key names a file mentions, live or commented.
//
// Commented counts on purpose: a key that is mutually exclusive with another —
// `storage.master_key.file` against the key_slots fields, which validation
// refuses to see set together — can only be documented as a commented block,
// and a gate that demanded a live node would be asking for a file that does not
// validate.
// The optional "- " is for a key that opens a list entry, which is how an
// allowlist's own fields are written.
var documentedKey = regexp.MustCompile(`(?m)^\s*#?\s*(?:-\s+)?([a-z0-9_]+):`)

func documentedKeys(contents []byte) map[string]struct{} {
	found := map[string]struct{}{}
	for _, match := range documentedKey.FindAllStringSubmatch(string(contents), -1) {
		found[match[1]] = struct{}{}
	}
	return found
}

// TestTheConfigurationReferenceDocumentsEveryKey holds the file whose own
// package comment calls it "the complete annotated configuration reference" to
// that claim.
func TestTheConfigurationReferenceDocumentsEveryKey(t *testing.T) {
	assertDocumentsEveryConfigKey(t, "configs/config.example.yaml", referenceconfigs.ExampleYAML)
}

// TestTheFirstRunTemplateDocumentsEveryKey holds the file an operator's own
// config.yaml is created from. A key missing here is a key nobody discovers by
// reading the file they were given.
func TestTheFirstRunTemplateDocumentsEveryKey(t *testing.T) {
	assertDocumentsEveryConfigKey(t, "internal/config/default.yaml", config.FirstRunTemplate())
}

func assertDocumentsEveryConfigKey(t *testing.T, name string, contents []byte) {
	t.Helper()
	documented := documentedKeys(contents)
	var missing []string
	for _, key := range configKeysFromStruct(t) {
		if _, ok := documented[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%s does not mention %d configuration key(s): %v\n"+
			"document each one, commented out where it is mutually exclusive with another mode",
			name, len(missing), missing)
	}
}
