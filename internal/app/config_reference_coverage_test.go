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
// path it can produce, including the omitempty ones that never appear in a
// marshalled Default() and are therefore invisible to the value gates.
//
// Paths, not bare names. The first version of this gate collected names, so a
// key was "documented" if a key of the same name existed anywhere in the file:
// `tls.certificates[].cert_file` passed for months on the strength of
// `metrics.tls.cert_file`, and the shape of a TLS certificate entry was written
// down nowhere. A list element's fields are spelled `list[].field`.
func configKeysFromStruct(t *testing.T) []string {
	t.Helper()
	seen := map[string]struct{}{}
	var walk func(reflect.Type, string)
	walk = func(structType reflect.Type, prefix string) {
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
			path := prefix + name
			seen[path] = struct{}{}
			fieldType := field.Type
			for fieldType.Kind() == reflect.Pointer || fieldType.Kind() == reflect.Slice {
				if fieldType.Kind() == reflect.Slice {
					path += "[]"
				}
				fieldType = fieldType.Elem()
			}
			// Duration and the other named scalars are structs to nobody.
			if fieldType.Kind() == reflect.Struct && fieldType.PkgPath() != "time" {
				walk(fieldType, path+".")
			}
		}
	}
	walk(reflect.TypeOf(config.Config{}), "")
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// documentedKeys returns the key paths a file mentions, live or commented.
//
// Commented counts on purpose: a key that is mutually exclusive with another —
// `storage.master_key.file` against the key_slots fields, which validation
// refuses to see set together — can only be documented as a commented block,
// and a gate that demanded a live node would be asking for a file that does not
// validate.
//
// Nesting is read from indentation. A commented line is measured as if the
// "# " were not there, which is how both files indent a commented block: at the
// column the live key would have. A line that opens a list entry ("- key:")
// nests its key one level under an element of the list above it, and the
// entry's other keys line up with that first one. Prose comments do not match,
// because a key is lower-case and stands at the start of its comment.
var documentedKey = regexp.MustCompile(`^( *)(?:# ?( *))?(- +)?([a-z0-9_]+):(?:\s|$)`)

func documentedKeys(contents []byte) map[string]struct{} {
	type level struct {
		indent int
		path   string
	}
	found := map[string]struct{}{}
	var stack []level
	parentAt := func(indent int) string {
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		if len(stack) == 0 {
			return ""
		}
		return stack[len(stack)-1].path + "."
	}
	for _, line := range strings.Split(string(contents), "\n") {
		match := documentedKey.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		indent := len(match[1]) + len(match[2])
		if dash := match[3]; dash != "" {
			// The list element sits at the dash; its key one dash further in.
			parent := strings.TrimSuffix(parentAt(indent), ".")
			stack = append(stack, level{indent: indent, path: parent + "[]"})
			indent += len(dash)
		}
		path := parentAt(indent) + match[4]
		found[path] = struct{}{}
		stack = append(stack, level{indent: indent, path: path})
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
