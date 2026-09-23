package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/config"
)

// migrateConfigCommand applies the mechanical half of the configuration
// retirement table to a file on disk.
//
// It is not a compatibility layer and it never runs on start. The runtime reads
// no retired key: this rewrites the file so the retired key stops existing, at
// a moment the operator chose. Starting Halro would be the wrong moment —
// quietly reinterpreting a security knob during a container rollout is worse
// than refusing to start, and a file rewritten under an operator's feet takes
// the way back to the older binary with it.
func migrateConfigCommand(path string, write bool) error {
	source, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	result, err := config.Migrate(source)
	if err != nil {
		return err
	}
	if len(result.Refusals) > 0 {
		// Laid out here rather than returned as one error: the failure reporter
		// prints an error a line at a time, and a reason that wraps onto
		// several lines would come back as several problems.
		fmt.Fprintln(os.Stderr, "halro: this configuration needs a decision before it can be migrated:")
		for _, refusal := range result.Refusals {
			fmt.Fprintf(os.Stderr, "  - %s\n", refusal)
		}
		return errSilentRefusal
	}
	if len(result.Actions) == 0 {
		fmt.Fprintln(os.Stdout, "nothing to migrate: the configuration holds no retired key")
		return nil
	}

	printMigrationDiff(os.Stdout, result)
	if !write {
		fmt.Fprintln(os.Stdout, "\nnothing written. Run again with --write to apply it.")
		return nil
	}

	backup := path + ".before-migrate"
	if _, err := os.Stat(backup); err == nil {
		return fmt.Errorf("%s already exists; move it aside so this run cannot overwrite the copy an earlier one kept", backup)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", backup, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat config: %w", err)
	}
	if err := os.WriteFile(backup, source, info.Mode().Perm()); err != nil {
		return fmt.Errorf("keep the original: %w", err)
	}
	if err := writeFileAtomically(path, result.Output, info.Mode().Perm()); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "\nwritten to %s; the original is %s\n", path, backup)
	return nil
}

// writeFileAtomically replaces a configuration file through a rename, so an
// interrupted run leaves the original rather than half a file. Unlike the data
// directory this is not fsynced: the file is the operator's own declaration and
// a copy of it sits beside it.
func writeFileAtomically(path string, content []byte, mode os.FileMode) error {
	staging, err := os.CreateTemp(filepath.Dir(path), ".halro-config-*")
	if err != nil {
		return fmt.Errorf("stage the migrated config: %w", err)
	}
	stagingPath := staging.Name()
	defer os.Remove(stagingPath)
	if _, err := staging.Write(content); err != nil {
		staging.Close()
		return fmt.Errorf("write the migrated config: %w", err)
	}
	if err := staging.Close(); err != nil {
		return fmt.Errorf("write the migrated config: %w", err)
	}
	if err := os.Chmod(stagingPath, mode); err != nil {
		return fmt.Errorf("write the migrated config: %w", err)
	}
	if err := os.Rename(stagingPath, path); err != nil {
		return fmt.Errorf("replace the config: %w", err)
	}
	return nil
}

func printMigrationDiff(out io.Writer, result config.MigrationResult) {
	for _, action := range result.Actions {
		fmt.Fprintf(out, "%s\n", action.Description)
		for _, line := range action.Removed {
			fmt.Fprintf(out, "  - %s\n", line)
		}
		for _, line := range action.Added {
			fmt.Fprintf(out, "  + %s\n", line)
		}
	}
}

// errSilentRefusal is returned by a command that has already printed why it
// refused, so the failure reporter sets the exit status without printing a
// second, flatter copy.
var errSilentRefusal = errors.New("")
