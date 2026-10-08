package ownedfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCreationAliasHasIndependentRetirementAndNeverOverwrites(t *testing.T) {
	directory := t.TempDir()
	original := filepath.Join(directory, "temporary")
	file, owner, err := Create(original, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("owned")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(directory, "published")
	alias, err := owner.LinkCaptured(destination)
	if err != nil || alias == nil {
		t.Fatal(err)
	}
	if err := owner.Retire(); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(destination); err != nil || string(raw) != "owned" {
		t.Fatal("original retirement removed alias", err)
	}
	if _, err := owner.LinkCaptured(filepath.Join(directory, "stale")); !errors.Is(err, ErrReplaced) {
		t.Fatal("retired receipt was reactivated", err)
	}
	if err := os.Rename(destination, destination+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := alias.Retire(); !errors.Is(err, ErrReplaced) {
		t.Fatal("alias retired replacement", err)
	}
	if raw, err := os.ReadFile(destination); err != nil || string(raw) != "foreign" {
		t.Fatal(err)
	}
	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(destination+".original", destination); err != nil {
		t.Fatal(err)
	}
	if err := alias.Retire(); err != nil {
		t.Fatal(err)
	}
	file, owner, err = Create(original, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if alias, err := owner.LinkCaptured(destination); !errors.Is(err, os.ErrExist) || alias != nil {
		t.Fatal("overwrote existing destination", alias, err)
	}
	if err := owner.Retire(); err != nil {
		t.Fatal(err)
	}
}
