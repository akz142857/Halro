package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// consumedByAccessor names fields that no caller reaches by name because the
// package exposes them through a method instead. Each entry needs the accessor
// written next to it, so the exemption stays checkable rather than becoming a
// place to park the next dead knob.
var consumedByAccessor = map[string]string{
	// SourceRateLimit.SourceRequestsPerMinute() applies the default for an
	// absent key, so callers must not read the pointer directly.
	"RequestsPerMinute": "SourceRateLimit.SourceRequestsPerMinute",
}

// refusedOnSight names sections that exist only so an operator who still has
// them gets a sentence instead of a bare decoding error. Nothing reads their
// values and nothing should: the whole section is rejected. Each entry carries
// the text Validate must produce, so an exemption cannot outlive the refusal it
// claims to stand for — delete the check and this test says so.
var refusedOnSight = map[string]string{
	"CircuitBreaker": "circuit_breaker has been replaced by routing",
}

// TestEveryConfigFieldIsReadSomewhere refuses a setting the binary does not
// act on. gateway.stream_idle_timeout survived for a long time as a field that
// was declared, defaulted, validated as positive and documented in the
// reference as "the inactivity period after which a streaming response is
// terminated" — while nothing anywhere read it. An operator who set it changed
// nothing and had no way to find that out.
//
// The check is a source scan rather than real static analysis, so it is a floor
// and not a proof: a generic field name like Enabled matches something
// everywhere. It catches the case that actually happened, which is a distinctly
// named knob wired to nothing.
func TestEveryConfigFieldIsReadSomewhere(t *testing.T) {
	source := readModuleSource(t)
	var walk func(reflect.Type, string)
	walk = func(typ reflect.Type, path string) {
		for index := range typ.NumField() {
			field := typ.Field(index)
			if strings.Split(field.Tag.Get("yaml"), ",")[0] == "" {
				continue
			}
			if refusal, retired := refusedOnSight[field.Name]; retired {
				// Against this package's own source: the refusal is Validate's
				// job, and the module scan deliberately skips internal/config.
				if !strings.Contains(configPackageSource(t), refusal) {
					t.Errorf("%s%s is exempted as a section that is refused, but nothing refuses it",
						path, field.Name)
				}
				continue
			}
			if field.Type.Kind() == reflect.Struct && field.Type.Name() != "Duration" {
				walk(field.Type, path+field.Name+".")
				continue
			}
			if accessor, exempt := consumedByAccessor[field.Name]; exempt {
				if !regexp.MustCompile(`\b` + regexp.QuoteMeta(accessor) + `\(`).MatchString(source) {
					t.Errorf("%s%s is exempted as read through %s(), which no longer exists",
						path, field.Name, accessor)
				}
				continue
			}
			if !regexp.MustCompile(`\.` + regexp.QuoteMeta(field.Name) + `\b`).MatchString(source) {
				t.Errorf("config field %s%s is read by nothing outside internal/config: either wire it up or delete it, "+
					"because a setting that changes nothing is worse than an absent one", path, field.Name)
			}
		}
	}
	walk(reflect.TypeOf(Config{}), "")
}

// readModuleSource returns every non-test Go file outside this package. Tests
// are excluded on purpose: a field only a test sets is still a field nothing
// acts on.
// configPackageSource is this package's own non-test source, for the one check
// that is about what internal/config does rather than about what reads it.
func configPackageSource(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var builder strings.Builder
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		payload, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		builder.Write(payload)
		builder.WriteByte('\n')
	}
	return builder.String()
}

func readModuleSource(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var builder strings.Builder
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "data", "dist", "web":
				return fs.SkipDir
			}
			if path == filepath.Join(root, "internal", "config") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		builder.Write(payload)
		builder.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if builder.Len() == 0 {
		t.Fatal("no source was scanned; the module root moved")
	}
	return builder.String()
}
