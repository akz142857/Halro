package deadman

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type auditWriter struct {
	mu   sync.Mutex
	path string
}

type auditSink interface {
	append(auditRecord) error
}

type auditRecord struct {
	OccurredAt time.Time `json:"occurred_at"`
	Action     string    `json:"action"`
	Outcome    string    `json:"outcome"`
	TargetID   string    `json:"target_id,omitempty"`
	EventID    string    `json:"event_id,omitempty"`
	Sequence   uint64    `json:"sequence,omitempty"`
	ReasonCode string    `json:"reason_code,omitempty"`
}

func (a *auditWriter) append(record auditRecord) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	if err := encoder.Encode(record); err != nil {
		file.Close()
		return fmt.Errorf("append audit record: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
