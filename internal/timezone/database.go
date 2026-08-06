// Package timezone reports where the process resolves IANA time zones from and
// how those rules actually behave.
//
// Period boundaries for daily budgets are derived from IANA rules, so two nodes
// resolving zones from different tzdata releases can place the same instant in
// different periods. That divergence is silent: nothing in the ledger records
// which rule set produced a boundary. This package makes the rule set an
// observable property so a fleet can assert that every node agrees.
package timezone

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// SourceEnv, SourceSystem and SourceEmbedded mirror the lookup order the Go
// runtime uses in time.LoadLocation: the ZONEINFO environment variable, then
// the well-known system directories, then the database embedded by importing
// time/tzdata.
const (
	SourceEnv      = "env"
	SourceSystem   = "system"
	SourceEmbedded = "embedded"
)

// UnknownVersion is reported when the resolved database publishes no version
// marker. The fingerprint remains meaningful in that case and is the value to
// compare across nodes.
const UnknownVersion = "unknown"

// systemZoneinfoDirs matches the paths the Go runtime searches on Unix hosts.
var systemZoneinfoDirs = []string{
	"/usr/share/zoneinfo/",
	"/usr/share/lib/zoneinfo/",
	"/usr/lib/locale/TZ/",
}

// baselineZones spans the rule shapes that break naive period arithmetic: no
// DST at all, a whole-hour southern-hemisphere transition, a half-hour
// transition, and the two northern schedules that differ by a week. A change to
// any of them changes the fingerprint.
var baselineZones = []string{
	"UTC",
	"Asia/Shanghai",
	"Europe/Berlin",
	"America/New_York",
	"America/Santiago",
	"Australia/Lord_Howe",
}

// Database describes the time zone rules this process resolves against.
type Database struct {
	// Source is one of SourceEnv, SourceSystem or SourceEmbedded.
	Source string `json:"source"`
	// Path is the directory the rules were read from. Empty for the embedded
	// database, which has no filesystem location.
	Path string `json:"path,omitempty"`
	// Version is the tzdata release, best effort. Distributions publish it in
	// different places and the embedded database publishes it nowhere, so
	// UnknownVersion is a normal value rather than an error.
	Version string `json:"version"`
	// Fingerprint digests the transition table of the zones described. It is
	// the authoritative cross-node comparison: it is derived from the rules
	// themselves, so it is defined even when Version is unknown.
	Fingerprint string `json:"fingerprint"`
	// Zones lists which zones the fingerprint covers, sorted and deduplicated.
	Zones []string `json:"zones"`
}

// fingerprintFrom and fingerprintTo bound the transition scan. The lower bound
// predates any instant this system can account for; the upper bound is far
// enough out that a release changing only future rules still moves the digest.
var (
	fingerprintFrom = time.Date(1970, time.January, 1, 0, 0, 0, 0, time.UTC)
	fingerprintTo   = time.Date(2050, time.January, 1, 0, 0, 0, 0, time.UTC)
)

var (
	describeOnce sync.Once
	describeInfo Database
	describeErr  error
)

// Describe reports the resolved database, covering the baseline zones plus any
// extra zones supplied — pass the accounting timezone so the fingerprint covers
// the rules actually in use. The result is computed once and cached; extra
// zones from later calls do not change the cached value.
func Describe(extraZones ...string) (Database, error) {
	describeOnce.Do(func() {
		describeInfo, describeErr = describe(extraZones)
	})
	return describeInfo, describeErr
}

func describe(extraZones []string) (Database, error) {
	zones := mergeZones(extraZones)
	source, path := resolveSource()
	fingerprint, err := Fingerprint(zones)
	if err != nil {
		return Database{}, err
	}
	return Database{
		Source:      source,
		Path:        path,
		Version:     readVersion(path),
		Fingerprint: fingerprint,
		Zones:       zones,
	}, nil
}

func mergeZones(extra []string) []string {
	seen := make(map[string]struct{}, len(baselineZones)+len(extra))
	merged := make([]string, 0, len(baselineZones)+len(extra))
	for _, zone := range append(append([]string{}, baselineZones...), extra...) {
		zone = strings.TrimSpace(zone)
		if zone == "" {
			continue
		}
		if _, ok := seen[zone]; ok {
			continue
		}
		seen[zone] = struct{}{}
		merged = append(merged, zone)
	}
	sort.Strings(merged)
	return merged
}

// resolveSource replicates the runtime's lookup order. It reports where
// time.LoadLocation will read from, not where it could read from.
func resolveSource() (string, string) {
	if dir := os.Getenv("ZONEINFO"); dir != "" {
		if _, err := os.Stat(dir); err == nil {
			return SourceEnv, dir
		}
	}
	for _, dir := range systemZoneinfoDirs {
		if info, err := os.Stat(filepath.Join(dir, "UTC")); err == nil && !info.IsDir() {
			return SourceSystem, strings.TrimSuffix(dir, "/")
		}
	}
	return SourceEmbedded, ""
}

// readVersion looks for the markers distributions ship alongside the rules.
// Neither is guaranteed to exist, and the embedded database has neither.
func readVersion(dir string) string {
	if dir == "" {
		return UnknownVersion
	}
	if version := readVersionFile(filepath.Join(dir, "+VERSION")); version != "" {
		return version
	}
	if version := readZicVersion(filepath.Join(dir, "tzdata.zi")); version != "" {
		return version
	}
	return UnknownVersion
}

func readVersionFile(path string) string {
	// Version markers are a single short line; cap the read so a wrong path
	// pointing at something large cannot be pulled into memory.
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, 64))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(content))
}

// readZicVersion parses the leading "# version 2025b" comment of a compiled
// zone information source file.
func readZicVersion(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, 4096))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 3 && fields[0] == "#" && fields[1] == "version" {
			return fields[2]
		}
	}
	return ""
}

// Fingerprint digests the UTC offset transitions of the given zones between
// fingerprintFrom and fingerprintTo. Two processes produce the same value if
// and only if they agree on every boundary those rules can place.
func Fingerprint(zones []string) (string, error) {
	digest := sha256.New()
	for _, name := range zones {
		location, err := time.LoadLocation(name)
		if err != nil {
			return "", fmt.Errorf("load zone %q: %w", name, err)
		}
		fmt.Fprintf(digest, "zone %s\n", name)
		_, offset := fingerprintFrom.In(location).Zone()
		fmt.Fprintf(digest, "base %d\n", offset)
		for _, change := range transitions(location) {
			fmt.Fprintf(digest, "at %d %d\n", change.at.Unix(), change.offset)
		}
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}

type transition struct {
	at     time.Time
	offset int
}

// transitions walks the scan window a month at a time and bisects any month
// whose endpoints disagree. Go exposes no accessor for a Location's transition
// table, so the rules have to be probed. A month step cannot separate two
// transitions inside one month; no zone in baselineZones has ever scheduled
// two, and a zone that did would still be detected as changed because the
// surviving transition's instant would move.
func transitions(location *time.Location) []transition {
	found := make([]transition, 0, 256)
	cursor := fingerprintFrom
	_, previous := cursor.In(location).Zone()
	for cursor.Before(fingerprintTo) {
		next := cursor.AddDate(0, 1, 0)
		if next.After(fingerprintTo) {
			next = fingerprintTo
		}
		_, offset := next.In(location).Zone()
		if offset != previous {
			at := bisect(location, cursor, next, previous)
			_, atOffset := at.In(location).Zone()
			found = append(found, transition{at: at, offset: atOffset})
			previous = offset
		}
		cursor = next
	}
	return found
}

// bisect narrows to the first second whose offset differs from before. low is
// known to carry before, high is known not to.
//
// The search runs on whole Unix seconds rather than on time.Time midpoints:
// halving a duration leaves a sub-second remainder, which would report a
// transition instant that is merely near the real one and make the digest
// depend on the width of the window that happened to contain it.
func bisect(location *time.Location, low, high time.Time, before int) time.Time {
	lowSecond, highSecond := low.Unix(), high.Unix()
	for highSecond-lowSecond > 1 {
		middle := lowSecond + (highSecond-lowSecond)/2
		if _, offset := time.Unix(middle, 0).In(location).Zone(); offset == before {
			lowSecond = middle
		} else {
			highSecond = middle
		}
	}
	return time.Unix(highSecond, 0).UTC()
}
