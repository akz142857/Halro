package replication

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/durable"
)

type StateBootstrap struct {
	Version     int
	Incarnation string
}

// ReadStateBootstrap reads only the unauthenticated fields needed to derive
// the cluster key. Its output is never authority: callers must immediately use
// that derived key to authenticate the complete state with ReadState. Keeping
// this pass bounded prevents a corrupt state file from controlling allocation
// before its MAC can be checked.
func ReadStateBootstrap(path string) (StateBootstrap, error) {
	file, err := os.Open(path)
	if err != nil {
		return StateBootstrap{}, fmt.Errorf("open member state bootstrap: %w", err)
	}
	defer file.Close()
	encoded, err := io.ReadAll(io.LimitReader(file, MaxMemberStateJSON+1))
	if err != nil {
		return StateBootstrap{}, fmt.Errorf("read member state bootstrap: %w", err)
	}
	if len(encoded) == 0 || len(encoded) > MaxMemberStateJSON {
		return StateBootstrap{}, errors.New("member-state bootstrap is empty or exceeds its size bound")
	}
	var bootstrap struct {
		Version     int    `json:"version"`
		Incarnation string `json:"incarnation"`
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	if err := decoder.Decode(&bootstrap); err != nil {
		return StateBootstrap{}, fmt.Errorf("decode member state bootstrap: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return StateBootstrap{}, errors.New("member-state bootstrap has trailing content")
	}
	if bootstrap.Version != StateVersion && bootstrap.Version != TransitionStateVersion {
		return StateBootstrap{}, fmt.Errorf("unsupported member-state version %d", bootstrap.Version)
	}
	if len(bootstrap.Incarnation) == 0 || len(bootstrap.Incarnation) > MaxIdentityBytes {
		return StateBootstrap{}, errors.New("member-state bootstrap incarnation is invalid")
	}
	return StateBootstrap{Version: bootstrap.Version, Incarnation: bootstrap.Incarnation}, nil
}

// ReadStateWithMasterKey performs the two-pass member-state open: a bounded,
// non-authoritative read supplies the HKDF salt, then the derived key verifies
// every authoritative field and the state MAC.
func ReadStateWithMasterKey(path string, masterKey []byte) (MemberState, error) {
	bootstrap, err := ReadStateBootstrap(path)
	if err != nil {
		return MemberState{}, err
	}
	key, err := DeriveClusterKey(masterKey, bootstrap.Incarnation)
	if err != nil {
		return MemberState{}, err
	}
	defer clear(key[:])
	return ReadState(path, key[:])
}

func ReadState(path string, key []byte) (MemberState, error) {
	file, err := os.Open(path)
	if err != nil {
		return MemberState{}, fmt.Errorf("read member state: %w", err)
	}
	defer file.Close()
	encoded, err := io.ReadAll(io.LimitReader(file, MaxMemberStateJSON+1))
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
