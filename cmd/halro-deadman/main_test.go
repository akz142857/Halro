package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/buildinfo"
)

// The dead-man is deployed on its own host, on its own schedule, and is the one
// component that stays up when Halro does not. An operator holding a probe that
// went quiet has to be able to ask it what it is before asking why it stopped.
func TestVersionFlagReportsBuildIdentityWithoutLoadingConfig(t *testing.T) {
	stdout := os.Stdout
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = write
	runErr := run([]string{"-version", "-config", "/nonexistent/halro-deadman.yaml"})
	os.Stdout = stdout
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	if runErr != nil {
		t.Fatalf("version refused to answer: %v", runErr)
	}
	payload, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	var info buildinfo.Info
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(payload))), &info); err != nil {
		t.Fatalf("version output is not the build identity: %v (%q)", err, payload)
	}
	if info.Version != buildinfo.Version || info.Commit != buildinfo.Commit || info.Date != buildinfo.Date {
		t.Fatalf("version reported %#v, build says %#v", info, buildinfo.Current())
	}
}

// A deployment gate that builds its flag list from configuration can end up
// passing both. Answering the version and exiting zero would tell it the
// configuration validated when nothing was read.
func TestVersionDoesNotSatisfyAConfigCheck(t *testing.T) {
	if err := run([]string{"-check-config", "-version", "-config", "/nonexistent/halro-deadman.yaml"}); err == nil {
		t.Fatal("-version answered a config check against a file that does not exist")
	}
}
