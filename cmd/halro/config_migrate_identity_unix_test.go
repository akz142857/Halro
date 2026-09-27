//go:build unix

package main

import (
	"os"
	"syscall"
	"testing"
)

func TestMigratePreservesRegularFileOwnerAndGroup(t *testing.T) {
	path := writeConfig(t, releasedConfig(t, "v0.8.5"))
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	want := before.Sys().(*syscall.Stat_t)
	if err := migrateConfigCommand(path, true); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, path + ".before-migrate"} {
		info, err := os.Stat(candidate)
		if err != nil {
			t.Fatal(err)
		}
		got := info.Sys().(*syscall.Stat_t)
		if got.Uid != want.Uid || got.Gid != want.Gid {
			t.Errorf("%s owner=%d:%d, want %d:%d", candidate, got.Uid, got.Gid, want.Uid, want.Gid)
		}
	}
}
