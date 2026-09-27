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
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat config: %w", err)
	}
	if write && pathInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to replace a symbolic-link configuration: migrate its regular-file target explicitly")
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return fmt.Errorf("stat config: %w", err)
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return errors.New("configuration is not a regular file")
	}
	source, err := io.ReadAll(file)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
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
	currentInfo, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat config: %w", err)
	}
	if currentInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, currentInfo) {
		return errors.New("configuration file changed identity while migration was being prepared; retry against the current file")
	}
	identity := configFileIdentity(info)
	if err := writeConfigBackup(backup, source, info.Mode().Perm(), identity); err != nil {
		return fmt.Errorf("keep the original: %w", err)
	}
	if err := writeFileAtomically(path, result.Output, info.Mode().Perm(), identity); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "\nwritten to %s; the original is %s\n", path, backup)
	return nil
}

// writeConfigBackup creates the recovery copy without an overwrite window and
// makes both its contents and directory entry durable before the original is
// replaced.
func writeConfigBackup(path string, content []byte, mode os.FileMode, identity fileIdentity) error {
	backup, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	kept := false
	defer func() {
		if kept {
			return
		}
		_ = backup.Close()
		_ = os.Remove(path)
		_ = syncDirectory(filepath.Dir(path))
	}()
	if err := writeConfiguredFile(backup, content, mode, identity); err != nil {
		return err
	}
	if err := backup.Close(); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	kept = true
	return nil
}

// writeFileAtomically replaces a regular configuration file through a durable
// rename. Its POSIX mode and owner/group follow the file being replaced; ACLs
// and extended attributes are deliberately outside this portable command's
// contract, so operators relying on them must re-apply them after migration.
func writeFileAtomically(path string, content []byte, mode os.FileMode, identity fileIdentity) error {
	staging, err := os.CreateTemp(filepath.Dir(path), ".halro-config-*")
	if err != nil {
		return fmt.Errorf("stage the migrated config: %w", err)
	}
	stagingPath := staging.Name()
	defer os.Remove(stagingPath)
	if err := writeConfiguredFile(staging, content, mode, identity); err != nil {
		staging.Close()
		return fmt.Errorf("write the migrated config: %w", err)
	}
	if err := staging.Close(); err != nil {
		return fmt.Errorf("write the migrated config: %w", err)
	}
	if err := os.Rename(stagingPath, path); err != nil {
		return fmt.Errorf("replace the config: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync the config directory: %w", err)
	}
	return nil
}

func writeConfiguredFile(file *os.File, content []byte, mode os.FileMode, identity fileIdentity) error {
	if _, err := file.Write(content); err != nil {
		return err
	}
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if err := applyConfigFileIdentity(file, identity); err != nil {
		return err
	}
	return syncConfigFile(file)
}

var syncConfigFile = func(file *os.File) error { return file.Sync() }

var syncDirectory = func(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
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
