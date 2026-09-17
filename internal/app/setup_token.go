package app

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"
)

const (
	setupTokenPrefix     = "setup_"
	setupTokenRandomSize = 32
	setupTokenSize       = len(setupTokenPrefix) + 43
	setupTokenInputLimit = 128
	setupTokenFileLimit  = 256
	setupTokenFileTTL    = 30 * time.Minute
	setupTokenMaxTTL     = 24 * time.Hour
)

var setupTokenPattern = regexp.MustCompile(`^setup_[A-Za-z0-9_-]{43}$`)

type setupTokenSource uint8

const (
	setupTokenSourceNone setupTokenSource = iota
	setupTokenSourceGenerated
	setupTokenSourceFile
)

func (source setupTokenSource) String() string {
	switch source {
	case setupTokenSourceGenerated:
		return "generated"
	case setupTokenSourceFile:
		return "file"
	default:
		return "none"
	}
}

type setupTokenState struct {
	hash      [sha256.Size]byte
	present   bool
	expiresAt time.Time
	source    setupTokenSource
	// generated is retained only until the start command takes it for display.
	// File-backed tokens are never placed here.
	generated []byte
}

func (state *setupTokenState) clear() {
	clear(state.generated)
	state.generated = nil
	clear(state.hash[:])
	state.present = false
	state.expiresAt = time.Time{}
	state.source = setupTokenSourceNone
}

func generateSetupToken() ([]byte, error) {
	random := make([]byte, setupTokenRandomSize)
	if _, err := rand.Read(random); err != nil {
		return nil, fmt.Errorf("generate setup token: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(random)
	clear(random)
	token := []byte(setupTokenPrefix + encoded)
	if len(token) != setupTokenSize || !setupTokenPattern.Match(token) {
		clear(token)
		return nil, errors.New("generated setup token has invalid format")
	}
	return token, nil
}

func readSetupTokenFile(path string) ([]byte, time.Time, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, time.Time{}, errors.New("file is not readable")
	}
	defer file.Close()
	payload, err := io.ReadAll(io.LimitReader(file, setupTokenFileLimit+1))
	if err != nil {
		clear(payload)
		return nil, time.Time{}, errors.New("file is not readable")
	}
	if len(payload) > setupTokenFileLimit {
		clear(payload)
		return nil, time.Time{}, errors.New("file contains an invalid setup token envelope")
	}
	payload = bytes.TrimSuffix(payload, []byte("\n"))
	payload = bytes.TrimSuffix(payload, []byte("\r"))
	lines := bytes.Split(payload, []byte("\n"))
	if len(lines) != 2 || len(lines[0]) != setupTokenSize || !setupTokenPattern.Match(lines[0]) || !bytes.HasPrefix(lines[1], []byte("expires_at=")) {
		clear(payload)
		return nil, time.Time{}, errors.New("file contains an invalid setup token envelope")
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, string(bytes.TrimPrefix(lines[1], []byte("expires_at="))))
	if err != nil || expiresAt.IsZero() {
		clear(payload)
		return nil, time.Time{}, errors.New("file contains an invalid setup token envelope")
	}
	token := append([]byte(nil), lines[0]...)
	clear(payload)
	return token, expiresAt.UTC(), nil
}

func newSetupTokenState(source setupTokenSource, token []byte, expiresAt time.Time) setupTokenState {
	state := setupTokenState{
		hash:      sha256.Sum256(token),
		present:   true,
		expiresAt: expiresAt,
		source:    source,
	}
	if source == setupTokenSourceGenerated {
		state.generated = token
	} else {
		clear(token)
	}
	return state
}

// GenerateSetupTokenFile creates a new setup token file without ever writing
// the token to stdout, stderr, or a process argument.
func GenerateSetupTokenFile(path string) error {
	return GenerateSetupTokenFileWithTTL(path, setupTokenFileTTL)
}

func GenerateSetupTokenFileWithTTL(path string, ttl time.Duration) error {
	if ttl <= 0 || ttl > setupTokenMaxTTL {
		return errors.New("setup token file TTL must be positive and no greater than 24h")
	}
	token, err := generateSetupToken()
	if err != nil {
		return err
	}
	defer clear(token)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create setup token output: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = file.Close()
			_ = os.Remove(path)
		}
	}()
	payload := fmt.Appendf(nil, "%s\nexpires_at=%s\n", token, time.Now().UTC().Add(ttl).Format(time.RFC3339Nano))
	defer clear(payload)
	if _, err := file.Write(payload); err != nil {
		return fmt.Errorf("write setup token output: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync setup token output: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close setup token output: %w", err)
	}
	complete = true
	return nil
}
