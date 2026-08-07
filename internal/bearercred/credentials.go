// Package bearercred manages a rotatable bearer-token credential file: two
// active tokens at a time so a rotation overlaps, SHA-256 comparison in
// constant time, an append-only audit of every issue and revoke, and a file
// lock so two rotations cannot race.
//
// It was named metricsauth when /metrics was its only caller. The audit anchor
// endpoint (ADR 0015) then reused it — correctly, since a second credential
// file is all an independently rotated domain needs — and the package name
// stopped describing what it does. A name that has drifted from its job is how
// a package quietly becomes the place everything lands.
package bearercred

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const fileSchemaVersion = 1

var errAuditMissing = errors.New("bearer credential audit ledger is missing")

type State string

const (
	StateActive   State = "active"
	StateRetiring State = "retiring"
	StateRevoked  State = "revoked"
)

type Credential struct {
	Version   uint64     `json:"version"`
	TokenHash string     `json:"token_hash"`
	State     State      `json:"state"`
	CreatedAt time.Time  `json:"created_at"`
	RetireAt  *time.Time `json:"retire_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

type File struct {
	SchemaVersion int          `json:"schema_version"`
	Epoch         uint64       `json:"epoch"`
	Credentials   []Credential `json:"credentials"`
}

type Rotation struct {
	Version uint64
	Token   []byte
}

type AuditEvent struct {
	Sequence     uint64    `json:"sequence"`
	At           time.Time `json:"at"`
	Action       string    `json:"action"`
	Version      uint64    `json:"version"`
	PreviousHash string    `json:"previous_hash,omitempty"`
	Hash         string    `json:"hash"`
}

type AuditHead struct {
	Sequence uint64 `json:"sequence"`
	Hash     string `json:"hash"`
}

type fileSignature struct {
	modified int64
	size     int64
	exists   bool
}

type Authorizer struct {
	path          string
	mu            sync.Mutex
	file          File
	revoked       map[uint64]struct{}
	credentialSig fileSignature
	revocationSig fileSignature
}

func NewAuthorizer(path string) (*Authorizer, error) {
	authorizer := &Authorizer{path: path}
	if err := authorizer.refresh(); err != nil {
		return nil, err
	}
	return authorizer, nil
}

func (a *Authorizer) Authorize(token string, now time.Time) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	credentialSig, err := signature(a.path, false)
	if err != nil {
		return false, err
	}
	revocationSig, err := signature(revocationPath(a.path), true)
	if err != nil {
		return false, err
	}
	if credentialSig != a.credentialSig || revocationSig != a.revocationSig {
		if err := a.refreshLocked(); err != nil {
			return false, err
		}
	}
	return authorize(a.file, a.revoked, token, now), nil
}

func (a *Authorizer) refresh() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.refreshLocked()
}

func (a *Authorizer) refreshLocked() error {
	file, err := Load(a.path)
	if err != nil {
		return err
	}
	revoked, err := loadRevocations(a.path)
	if err != nil {
		return err
	}
	credentialSig, err := signature(a.path, false)
	if err != nil {
		return err
	}
	revocationSig, err := signature(revocationPath(a.path), true)
	if err != nil {
		return err
	}
	a.file, a.revoked = file, revoked
	a.credentialSig, a.revocationSig = credentialSig, revocationSig
	return nil
}

func signature(path string, optional bool) (fileSignature, error) {
	info, err := os.Stat(path)
	if optional && errors.Is(err, os.ErrNotExist) {
		return fileSignature{}, nil
	}
	if err != nil {
		return fileSignature{}, err
	}
	return fileSignature{modified: info.ModTime().UnixNano(), size: info.Size(), exists: true}, nil
}

func Load(path string) (File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return File{}, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return File{}, fmt.Errorf("bearer credential file permissions %04o are too broad", info.Mode().Perm())
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}
	var file File
	if err := json.Unmarshal(payload, &file); err != nil {
		return File{}, fmt.Errorf("decode bearer credential file: %w", err)
	}
	if err := file.Validate(); err != nil {
		return File{}, err
	}
	return file, nil
}

func (f File) Validate() error {
	if f.SchemaVersion != fileSchemaVersion {
		return fmt.Errorf("bearer credential schema version %d is not supported", f.SchemaVersion)
	}
	seen := make(map[uint64]struct{}, len(f.Credentials))
	active := 0
	for _, credential := range f.Credentials {
		if credential.Version == 0 || credential.Version > f.Epoch {
			return errors.New("bearer credential has an invalid version")
		}
		if _, exists := seen[credential.Version]; exists {
			return errors.New("bearer credential version is duplicated")
		}
		seen[credential.Version] = struct{}{}
		hash, err := hex.DecodeString(credential.TokenHash)
		if err != nil || len(hash) != sha256.Size {
			return errors.New("bearer credential has an invalid token hash")
		}
		switch credential.State {
		case StateActive:
			active++
			if credential.RetireAt != nil || credential.RevokedAt != nil {
				return errors.New("active bearer credential has retirement metadata")
			}
		case StateRetiring:
			if credential.RetireAt == nil || credential.RevokedAt != nil {
				return errors.New("retiring bearer credential has invalid metadata")
			}
		case StateRevoked:
			if credential.RevokedAt == nil {
				return errors.New("revoked bearer credential is missing revoked_at")
			}
		default:
			return errors.New("bearer credential has an invalid state")
		}
	}
	if active > 1 {
		return errors.New("bearer credential file has multiple active credentials")
	}
	return nil
}

func Authorize(path, token string, now time.Time) (bool, error) {
	file, err := Load(path)
	if err != nil {
		return false, err
	}
	revoked, err := loadRevocations(path)
	if err != nil {
		return false, err
	}
	return authorize(file, revoked, token, now), nil
}

func authorize(file File, revoked map[uint64]struct{}, token string, now time.Time) bool {
	candidate := sha256.Sum256([]byte(token))
	authorized := 0
	for _, credential := range file.Credentials {
		if _, permanentlyRevoked := revoked[credential.Version]; permanentlyRevoked ||
			credential.State == StateRevoked ||
			(credential.State == StateRetiring && !now.Before(*credential.RetireAt)) {
			continue
		}
		expected, _ := hex.DecodeString(credential.TokenHash)
		authorized |= subtle.ConstantTimeCompare(candidate[:], expected)
	}
	return authorized == 1
}

func Rotate(path string, overlap time.Duration, now time.Time) (Rotation, error) {
	var rotation Rotation
	err := withCredentialLock(path, func() error {
		var err error
		rotation, err = rotateLocked(path, overlap, now)
		return err
	})
	return rotation, err
}

func rotateLocked(path string, overlap time.Duration, now time.Time) (Rotation, error) {
	if overlap < 0 || overlap > 24*time.Hour {
		return Rotation{}, errors.New("bearer credential overlap must be between zero and 24 hours")
	}
	file, err := loadOrInitialize(path)
	if err != nil {
		return Rotation{}, err
	}
	revoked, err := loadRevocations(path)
	if err != nil {
		return Rotation{}, err
	}
	for version := range revoked {
		if version > file.Epoch {
			file.Epoch = version
		}
	}
	events, auditErr := loadAudit(path)
	if auditErr != nil && !(errors.Is(auditErr, errAuditMissing) && len(file.Credentials) == 0) {
		return Rotation{}, auditErr
	}
	for _, event := range events {
		if event.Version > file.Epoch {
			file.Epoch = event.Version
		}
	}
	retireAt := now.Add(overlap).UTC()
	for index := range file.Credentials {
		if file.Credentials[index].State == StateActive {
			file.Credentials[index].State = StateRetiring
			file.Credentials[index].RetireAt = &retireAt
		}
	}
	token := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, token); err != nil {
		return Rotation{}, fmt.Errorf("generate bearer credential: %w", err)
	}
	encoded := []byte(base64.RawURLEncoding.EncodeToString(token))
	clear(token)
	hash := sha256.Sum256(encoded)
	file.Epoch++
	file.Credentials = append(file.Credentials, Credential{
		Version: file.Epoch, TokenHash: hex.EncodeToString(hash[:]), State: StateActive,
		CreatedAt: now.UTC(),
	})
	if err := appendAudit(path, "rotate", file.Epoch, now); err != nil {
		clear(encoded)
		return Rotation{}, fmt.Errorf("append credential rotation audit: %w", err)
	}
	if err := write(path, file); err != nil {
		clear(encoded)
		return Rotation{}, err
	}
	return Rotation{Version: file.Epoch, Token: encoded}, nil
}

func Revoke(path string, version uint64, now time.Time) error {
	return withCredentialLock(path, func() error {
		return revokeLocked(path, version, now)
	})
}

func revokeLocked(path string, version uint64, now time.Time) error {
	file, err := Load(path)
	if err != nil {
		return err
	}
	if err := VerifyAudit(path); err != nil {
		return fmt.Errorf("verify bearer credential audit before revoke: %w", err)
	}
	found := false
	for index := range file.Credentials {
		if file.Credentials[index].Version != version {
			continue
		}
		found = true
		if file.Credentials[index].State == StateRevoked {
			return nil
		}
		revokedAt := now.UTC()
		file.Credentials[index].State = StateRevoked
		file.Credentials[index].RetireAt = nil
		file.Credentials[index].RevokedAt = &revokedAt
	}
	if !found {
		return fmt.Errorf("bearer credential version %d was not found", version)
	}
	if err := appendRevocation(path, version); err != nil {
		return err
	}
	if err := appendAudit(path, "revoke", version, now); err != nil {
		return fmt.Errorf("credential permanently revoked but audit append failed; inspect state before retrying: %w", err)
	}
	if err := write(path, file); err != nil {
		return fmt.Errorf("credential permanently revoked but state update failed: %w", err)
	}
	return nil
}

func revocationPath(path string) string { return path + ".revocations" }
func auditPath(path string) string      { return path + ".audit" }

func VerifyAudit(path string) error {
	_, err := AuditStatus(path)
	return err
}

func AuditStatus(path string) (AuditHead, error) {
	file, err := Load(path)
	if err != nil {
		return AuditHead{}, err
	}
	events, err := loadAudit(path)
	if err != nil {
		return AuditHead{}, err
	}
	rotated := make(map[uint64]struct{})
	revoked := make(map[uint64]struct{})
	for _, event := range events {
		switch event.Action {
		case "rotate":
			rotated[event.Version] = struct{}{}
		case "revoke":
			revoked[event.Version] = struct{}{}
		}
	}
	permanentRevocations, err := loadRevocations(path)
	if err != nil {
		return AuditHead{}, err
	}
	for _, credential := range file.Credentials {
		if _, ok := rotated[credential.Version]; !ok {
			return AuditHead{}, fmt.Errorf("bearer credential audit is missing rotate event for version %d", credential.Version)
		}
	}
	for version := range permanentRevocations {
		if _, ok := revoked[version]; !ok {
			return AuditHead{}, fmt.Errorf("bearer credential audit is missing revoke event for version %d", version)
		}
	}
	head := events[len(events)-1]
	return AuditHead{Sequence: head.Sequence, Hash: head.Hash}, nil
}

func loadAudit(path string) ([]AuditEvent, error) {
	path = auditPath(path)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errAuditMissing
	}
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("bearer credential audit permissions %04o are too broad", info.Mode().Perm())
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(payload)), "\n")
	events := make([]AuditEvent, 0, len(lines))
	previous := ""
	for index, line := range lines {
		if line == "" {
			continue
		}
		var event AuditEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil, fmt.Errorf("decode bearer credential audit event: %w", err)
		}
		if event.Sequence != uint64(index+1) || event.Version == 0 ||
			(event.Action != "rotate" && event.Action != "revoke") || event.PreviousHash != previous {
			return nil, errors.New("bearer credential audit sequence is invalid")
		}
		expected := auditHash(event.Sequence, event.At, event.Action, event.Version, event.PreviousHash)
		if subtle.ConstantTimeCompare([]byte(event.Hash), []byte(expected)) != 1 {
			return nil, errors.New("bearer credential audit hash is invalid")
		}
		previous = event.Hash
		events = append(events, event)
	}
	if len(events) == 0 {
		return nil, errors.New("bearer credential audit ledger is empty")
	}
	return events, nil
}

func appendAudit(path, action string, version uint64, now time.Time) error {
	events, err := loadAudit(path)
	if err != nil && !errors.Is(err, errAuditMissing) {
		return err
	}
	sequence := uint64(len(events) + 1)
	previous := ""
	if len(events) > 0 {
		previous = events[len(events)-1].Hash
	}
	event := AuditEvent{Sequence: sequence, At: now.UTC(), Action: action, Version: version, PreviousHash: previous}
	event.Hash = auditHash(event.Sequence, event.At, event.Action, event.Version, event.PreviousHash)
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(auditPath(path), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(append(payload, '\n')); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func auditHash(sequence uint64, at time.Time, action string, version uint64, previous string) string {
	payload := fmt.Sprintf("%d\n%s\n%s\n%d\n%s", sequence, at.UTC().Format(time.RFC3339Nano), action, version, previous)
	hash := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(hash[:])
}

func loadRevocations(path string) (map[uint64]struct{}, error) {
	revocationsPath := revocationPath(path)
	info, err := os.Stat(revocationsPath)
	if errors.Is(err, os.ErrNotExist) {
		return map[uint64]struct{}{}, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("metrics revocation ledger permissions %04o are too broad", info.Mode().Perm())
	}
	payload, err := os.ReadFile(revocationsPath)
	if err != nil {
		return nil, err
	}
	result := make(map[uint64]struct{})
	for _, line := range strings.Split(strings.TrimSpace(string(payload)), "\n") {
		if line == "" {
			continue
		}
		version, err := strconv.ParseUint(line, 10, 64)
		if err != nil || version == 0 {
			return nil, errors.New("bearer credential revocation ledger is invalid")
		}
		result[version] = struct{}{}
	}
	return result, nil
}

func appendRevocation(path string, version uint64) error {
	revoked, err := loadRevocations(path)
	if err != nil {
		return err
	}
	if _, exists := revoked[version]; exists {
		return nil
	}
	file, err := os.OpenFile(revocationPath(path), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := fmt.Fprintf(file, "%d\n", version); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func loadOrInitialize(path string) (File, error) {
	file, err := Load(path)
	if err == nil {
		return file, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return File{}, err
	}
	return File{SchemaVersion: fileSchemaVersion}, nil
}

func write(path string, file File) error {
	if err := file.Validate(); err != nil {
		return err
	}
	sort.Slice(file.Credentials, func(i, j int) bool {
		return file.Credentials[i].Version < file.Credentials[j].Version
	})
	payload, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".metrics-credentials-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(payload); err != nil {
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
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer directoryHandle.Close()
	return directoryHandle.Sync()
}
