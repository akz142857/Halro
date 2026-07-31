package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/id"
)

const (
	GatewayKeyPrefix      = "gw_"
	GatewayKeyRandomBytes = 32
	HashVersion           = 1
)

func GenerateGatewayKey(projectID, name string, expiresAt *time.Time) (string, domain.GatewayKey, error) {
	if projectID == "" {
		return "", domain.GatewayKey{}, errors.New("project id is required")
	}
	if strings.TrimSpace(name) == "" {
		return "", domain.GatewayKey{}, errors.New("key name is required")
	}
	random := make([]byte, GatewayKeyRandomBytes)
	if _, err := rand.Read(random); err != nil {
		return "", domain.GatewayKey{}, fmt.Errorf("generate gateway key: %w", err)
	}
	plaintext := GatewayKeyPrefix + base64.RawURLEncoding.EncodeToString(random)
	clear(random)
	keyID, err := id.New("key")
	if err != nil {
		return "", domain.GatewayKey{}, err
	}
	now := time.Now().UTC()
	key := domain.GatewayKey{
		ID:          keyID,
		ProjectID:   projectID,
		Name:        name,
		HashVersion: HashVersion,
		KeyHash:     HashGatewayKey(plaintext),
		Enabled:     true,
		ExpiresAt:   expiresAt,
		CreatedAt:   now,
	}
	return plaintext, key, nil
}

func HashGatewayKey(plaintext string) [32]byte {
	return sha256.Sum256([]byte(plaintext))
}

func ValidateGatewayKeyFormat(plaintext string) error {
	if !strings.HasPrefix(plaintext, GatewayKeyPrefix) {
		return errors.New("invalid gateway key prefix")
	}
	encoded := strings.TrimPrefix(plaintext, GatewayKeyPrefix)
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return errors.New("invalid gateway key encoding")
	}
	defer clear(decoded)
	if len(decoded) != GatewayKeyRandomBytes {
		return errors.New("invalid gateway key length")
	}
	return nil
}

func ConstantTimeHashMatch(expected, actual [32]byte) bool {
	return subtle.ConstantTimeCompare(expected[:], actual[:]) == 1
}
