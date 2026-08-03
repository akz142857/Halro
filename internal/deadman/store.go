package deadman

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func loadState(path string, outboxLimit int) (persistedState, error) {
	state := persistedState{Version: 1, Targets: make(map[string]TargetState)}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	maximumBytes := int64(1 << 20)
	if candidate := int64(outboxLimit) * (8 << 10); candidate > maximumBytes {
		maximumBytes = candidate
	}
	if info.Size() > maximumBytes {
		return persistedState{}, fmt.Errorf("persistent dead-man state exceeds %d bytes", maximumBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return persistedState{}, fmt.Errorf("decode persistent dead-man state: %w", err)
	}
	if state.Version != 1 || state.Targets == nil {
		return persistedState{}, fmt.Errorf("unsupported persistent dead-man state version %d", state.Version)
	}
	if len(state.Outbox) > outboxLimit {
		return persistedState{}, fmt.Errorf("persistent dead-man outbox has %d events, limit is %d", len(state.Outbox), outboxLimit)
	}
	var previousSequence uint64
	for index, queued := range state.Outbox {
		if queued.Event.EventID == "" || queued.Event.Sequence == 0 || queued.Event.Sequence > state.Sequence {
			return persistedState{}, fmt.Errorf("persistent dead-man outbox contains an invalid event")
		}
		if index > 0 && queued.Event.Sequence <= previousSequence {
			return persistedState{}, fmt.Errorf("persistent dead-man outbox sequence is not strictly increasing")
		}
		previousSequence = queued.Event.Sequence
	}
	return state, nil
}

func saveState(path string, state persistedState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".deadman-state-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
