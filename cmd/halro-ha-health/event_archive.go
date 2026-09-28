package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/akz142857/Halro/internal/durable"
)

const archiveRecordLimit = 1000

// This is evidence captured by the monitoring process, not an HA audit log.
// A member can fail or overflow its ring before the next independent poll.
type eventArchiveRecord struct {
	ObservedAt   time.Time               `json:"observed_at"`
	NodeID       string                  `json:"node_id"`
	Incarnation  string                  `json:"incarnation,omitempty"`
	Source       string                  `json:"source"`
	ProcessAt    time.Time               `json:"source_started_at,omitempty"`
	Gap          string                  `json:"gap,omitempty"`
	Live         *liveTransition         `json:"live,omitempty"`
	Availability *availabilityTransition `json:"availability,omitempty"`
	ReplicaStage *replicaStageTransition `json:"replica_stage,omitempty"`
}

type archiveCursor struct {
	ProcessAt   time.Time `json:"source_started_at,omitempty"`
	Incarnation string    `json:"incarnation,omitempty"`
	Sequence    uint64    `json:"sequence"`
	Failed      bool      `json:"failed,omitempty"`
	Active      bool      `json:"active,omitempty"`
}

type archiveDocument struct {
	Version          int                      `json:"version"`
	Environment      string                   `json:"environment"`
	Cluster          string                   `json:"cluster"`
	Members          []string                 `json:"members"`
	PolledAt         time.Time                `json:"polled_at,omitempty"`
	CollectionErrors []string                 `json:"collection_errors,omitempty"`
	RetentionDropped uint64                   `json:"retention_dropped"`
	Cursors          map[string]archiveCursor `json:"cursors"`
	Records          []eventArchiveRecord     `json:"records"`
}

type eventArchiveView struct {
	Status           string               `json:"status"`
	PolledAt         time.Time            `json:"polled_at,omitempty"`
	CollectionErrors []string             `json:"collection_errors,omitempty"`
	RetentionDropped uint64               `json:"retention_dropped,omitempty"`
	Records          []eventArchiveRecord `json:"records,omitempty"`
}

type eventArchive struct {
	mu      sync.Mutex
	path    string
	lock    *os.File
	doc     archiveDocument
	lastErr string
}

func openEventArchive(path, environment, cluster string, members []string) (*eventArchive, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("event-journal must be a clean absolute path")
	}
	copyMembers := append([]string(nil), members...)
	slices.Sort(copyMembers)
	doc := archiveDocument{Version: 1, Environment: environment, Cluster: cluster, Members: copyMembers, Cursors: map[string]archiveCursor{}, Records: []eventArchiveRecord{}}
	parent, parentErr := os.Lstat(filepath.Dir(path))
	if parentErr != nil || !parent.IsDir() || parent.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("event-journal parent must be a directory without group/world write access")
	}
	if lockPathInfo, lockErr := os.Lstat(path + ".lock"); lockErr == nil {
		if !lockPathInfo.Mode().IsRegular() || lockPathInfo.Mode().Perm() != 0o600 {
			return nil, errors.New("event-journal lock must be a private regular file")
		}
	} else if !errors.Is(lockErr, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect event-journal lock: %w", lockErr)
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open event-journal lock: %w", err)
	}
	owned := false
	defer func() {
		if !owned {
			_ = lock.Close()
		}
	}()
	lockInfo, err := lock.Stat()
	if err != nil || !lockInfo.Mode().IsRegular() || lockInfo.Mode().Perm() != 0o600 {
		return nil, errors.New("event-journal lock must be a private regular file")
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.New("event-journal is already owned by another service")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		owned = true
		return &eventArchive{path: path, lock: lock, doc: doc}, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > 4<<20 {
		return nil, errors.New("event-journal must be a private regular file at most 4 MiB")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read event-journal: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var existing archiveDocument
	if err := decoder.Decode(&existing); err != nil {
		return nil, fmt.Errorf("decode event-journal: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) || existing.Version != 1 || existing.Environment != environment || existing.Cluster != cluster ||
		!slices.Equal(existing.Members, copyMembers) || existing.Cursors == nil || len(existing.Records) > archiveRecordLimit {
		return nil, errors.New("event-journal identity, version or shape mismatch")
	}
	owned = true
	return &eventArchive{path: path, lock: lock, doc: existing}, nil
}

func (a *eventArchive) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lock == nil {
		return nil
	}
	err := a.lock.Close()
	a.lock = nil
	return err
}

func (s *server) pollArchive(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		s.pollArchiveOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *server) pollArchiveOnce(ctx context.Context) {
	if s.archive == nil || ctx.Err() != nil {
		return
	}
	statuses := s.collectStatuses(ctx)
	if ctx.Err() != nil {
		return
	}
	s.archive.capture(statuses, time.Now().UTC())
}

func (a *eventArchive) capture(statuses []memberStatus, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	doc := a.doc
	doc.Cursors = make(map[string]archiveCursor, len(a.doc.Cursors))
	for key, value := range a.doc.Cursors {
		doc.Cursors[key] = value
	}
	doc.Records = append([]eventArchiveRecord(nil), a.doc.Records...)
	doc.PolledAt = now
	doc.CollectionErrors = nil
	// The journal has its own fixed inventory. A shortened or duplicated
	// collector result must not refresh the journal while looking complete.
	byMember := make(map[string][]memberStatus, len(doc.Members))
	for _, status := range statuses {
		if !slices.Contains(doc.Members, status.NodeID) {
			doc.CollectionErrors = append(doc.CollectionErrors, "unexpected:"+status.NodeID)
			continue
		}
		byMember[status.NodeID] = append(byMember[status.NodeID], status)
	}
	for _, member := range doc.Members {
		status := memberStatus{NodeID: member, Error: "member_inventory"}
		if len(byMember[member]) == 1 {
			status = byMember[member][0]
		}
		if status.Error != "" {
			doc.CollectionErrors = append(doc.CollectionErrors, status.NodeID)
			for _, source := range []string{"state_publisher", "primary_coordinator", "replica_stage"} {
				key := status.NodeID + "/" + source
				cursor, exists := doc.Cursors[key]
				if source != "state_publisher" && (!exists || !cursor.Active) {
					continue
				}
				if !cursor.Failed {
					appendArchiveRecord(&doc, eventArchiveRecord{ObservedAt: now, NodeID: status.NodeID, Incarnation: cursor.Incarnation, Source: source, ProcessAt: cursor.ProcessAt, Gap: "collection_failed"})
					cursor.Failed = true
					doc.Cursors[key] = cursor
				}
			}
			continue
		}
		if status.LiveTransitions != nil {
			history := status.LiveTransitions
			events := make([]eventArchiveRecord, 0, len(history.Events))
			for i := range history.Events {
				event := history.Events[i]
				events = append(events, eventArchiveRecord{ObservedAt: now, NodeID: status.NodeID, Incarnation: status.Incarnation, Source: "state_publisher", ProcessAt: history.PublisherStartedAt, Live: &event})
			}
			captureSource(&doc, status.NodeID, status.Incarnation, "state_publisher", history.PublisherStartedAt, history.Dropped, events, now)
		}
		if status.AvailabilityTransitions != nil {
			history := status.AvailabilityTransitions
			events := make([]eventArchiveRecord, 0, len(history.Events))
			for i := range history.Events {
				event := history.Events[i]
				events = append(events, eventArchiveRecord{ObservedAt: now, NodeID: status.NodeID, Incarnation: status.Incarnation, Source: "primary_coordinator", ProcessAt: history.CoordinatorStartedAt, Availability: &event})
			}
			captureSource(&doc, status.NodeID, status.Incarnation, "primary_coordinator", history.CoordinatorStartedAt, history.Dropped, events, now)
		} else if cursor, exists := doc.Cursors[status.NodeID+"/primary_coordinator"]; exists {
			cursor.Active, cursor.Failed = false, false
			doc.Cursors[status.NodeID+"/primary_coordinator"] = cursor
		}
		if status.ReplicaStageTransitions != nil {
			history := status.ReplicaStageTransitions
			events := make([]eventArchiveRecord, 0, len(history.Events))
			for i := range history.Events {
				event := history.Events[i]
				events = append(events, eventArchiveRecord{ObservedAt: now, NodeID: status.NodeID, Incarnation: status.Incarnation, Source: "replica_stage", ProcessAt: history.ReceiverStartedAt, ReplicaStage: &event})
			}
			captureSource(&doc, status.NodeID, status.Incarnation, "replica_stage", history.ReceiverStartedAt, history.Dropped, events, now)
		} else if cursor, exists := doc.Cursors[status.NodeID+"/replica_stage"]; exists {
			cursor.Active, cursor.Failed = false, false
			doc.Cursors[status.NodeID+"/replica_stage"] = cursor
		}
	}
	if err := persistArchive(a.path, doc); err != nil {
		if a.lastErr == "" {
			log.Printf("HA event journal persistence failed: %v", err)
		}
		a.lastErr = "journal_write_failed"
		return
	}
	a.doc, a.lastErr = doc, ""
}

func captureSource(doc *archiveDocument, node, incarnation, source string, processAt time.Time, dropped uint64, events []eventArchiveRecord, now time.Time) {
	key := node + "/" + source
	cursor, exists := doc.Cursors[key]
	if !exists || cursor.ProcessAt.IsZero() {
		appendArchiveRecord(doc, eventArchiveRecord{ObservedAt: now, NodeID: node, Incarnation: incarnation, Source: source, ProcessAt: processAt, Gap: "initial_observation"})
		cursor.Sequence = 0
	} else if cursor.Incarnation != incarnation {
		appendArchiveRecord(doc, eventArchiveRecord{ObservedAt: now, NodeID: node, Incarnation: incarnation, Source: source, ProcessAt: processAt, Gap: "incarnation_changed"})
		cursor.Sequence = 0
	} else if !cursor.ProcessAt.Equal(processAt) {
		appendArchiveRecord(doc, eventArchiveRecord{ObservedAt: now, NodeID: node, Incarnation: incarnation, Source: source, ProcessAt: processAt, Gap: "source_changed"})
		cursor.Sequence = 0
	}
	cursor.Failed, cursor.Active = false, true
	if dropped > cursor.Sequence {
		appendArchiveRecord(doc, eventArchiveRecord{ObservedAt: now, NodeID: node, Incarnation: incarnation, Source: source, ProcessAt: processAt, Gap: "ring_history_missing"})
	}
	if end := dropped + uint64(len(events)); !cursor.ProcessAt.IsZero() && cursor.ProcessAt.Equal(processAt) && end < cursor.Sequence {
		appendArchiveRecord(doc, eventArchiveRecord{ObservedAt: now, NodeID: node, Incarnation: incarnation, Source: source, ProcessAt: processAt, Gap: "sequence_regressed"})
	}
	cursor.ProcessAt = processAt
	cursor.Incarnation = incarnation
	for _, record := range events {
		sequence := uint64(0)
		if record.Live != nil {
			sequence = record.Live.Sequence
		} else if record.Availability != nil {
			sequence = record.Availability.Sequence
		} else if record.ReplicaStage != nil {
			sequence = record.ReplicaStage.Sequence
		}
		if sequence > cursor.Sequence {
			appendArchiveRecord(doc, record)
			cursor.Sequence = sequence
		}
	}
	doc.Cursors[key] = cursor
}

func appendArchiveRecord(doc *archiveDocument, record eventArchiveRecord) {
	doc.Records = append(doc.Records, record)
	if excess := len(doc.Records) - archiveRecordLimit; excess > 0 {
		doc.Records = append([]eventArchiveRecord(nil), doc.Records[excess:]...)
		doc.RetentionDropped += uint64(excess)
	}
}

func persistArchive(path string, doc any) error {
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".ha-events-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if err := json.NewEncoder(file).Encode(doc); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	return durable.SyncDirectory(directory)
}

func (a *eventArchive) view() eventArchiveView {
	a.mu.Lock()
	defer a.mu.Unlock()
	status := "ok"
	if a.lastErr != "" {
		status = a.lastErr
	} else if a.doc.PolledAt.IsZero() || time.Since(a.doc.PolledAt) > 45*time.Second {
		status = "stale"
	} else if len(a.doc.CollectionErrors) > 0 {
		status = "partial"
	}
	return eventArchiveView{Status: status, PolledAt: a.doc.PolledAt, CollectionErrors: append([]string(nil), a.doc.CollectionErrors...), RetentionDropped: a.doc.RetentionDropped,
		Records: append([]eventArchiveRecord(nil), a.doc.Records...)}
}

func (s *server) archiveView() eventArchiveView {
	if s.archive == nil {
		return eventArchiveView{Status: "not_configured"}
	}
	return s.archive.view()
}

func (s *server) eventArchiveResponse(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(s.archiveView())
}
