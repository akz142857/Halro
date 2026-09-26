package replication

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/durable"
)

func ReadState(path string, key []byte) (MemberState, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return MemberState{}, fmt.Errorf("read member state: %w", err)
	}
	state, err := UnmarshalState(encoded, key)
	if err != nil {
		return MemberState{}, fmt.Errorf("authenticate member state: %w", err)
	}
	return state, nil
}

// WriteState publishes authenticated state through file fsync, rename and
// directory fsync. A crash before the rename leaves the previous state intact;
// a crash after it cannot lose the new directory entry while retaining a
// successful return.
func WriteState(path string, state MemberState, key []byte) error {
	encoded, err := MarshalState(state, key)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := ensureDurableDirectory(directory); err != nil {
		return fmt.Errorf("create member-state directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".state-*")
	if err != nil {
		return fmt.Errorf("create temporary member state: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := writeAll(temporary, encoded); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write member state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync member state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close member state: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("publish member state: %w", err)
	}
	if err := durable.SyncDirectory(directory); err != nil {
		return fmt.Errorf("publish member-state directory entry: %w", err)
	}
	return nil
}
