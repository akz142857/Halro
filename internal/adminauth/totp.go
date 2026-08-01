package adminauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // TOTP interoperability requires HMAC-SHA1; it is not used as a password hash.
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	TOTPPeriod           = int64(30)
	TOTPSecretBytes      = 20
	MFAChallengeAttempts = 5
)

func NewTOTPSecret() ([]byte, error) {
	secret := make([]byte, TOTPSecretBytes)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("generate TOTP secret: %w", err)
	}
	return secret, nil
}

func TOTPSecretBase32(secret []byte) string {
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
}

func TOTPUri(issuer, username string, secret []byte) string {
	label := issuer + ":" + username
	q := url.Values{"secret": {TOTPSecretBase32(secret)}, "issuer": {issuer}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return "otpauth://totp/" + url.PathEscape(label) + "?" + q.Encode()
}

func TOTPCode(secret []byte, step int64) string {
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	_, _ = mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := (binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff) % 1_000_000
	return fmt.Sprintf("%06d", value)
}

func VerifyTOTP(secret []byte, supplied string, now time.Time, lastAccepted int64) (int64, bool) {
	code := strings.ReplaceAll(strings.TrimSpace(supplied), " ", "")
	if len(code) != 6 {
		return 0, false
	}
	if _, err := strconv.Atoi(code); err != nil {
		return 0, false
	}
	current := now.UTC().Unix() / TOTPPeriod
	for _, step := range []int64{current, current - 1, current + 1} {
		if step <= lastAccepted {
			continue
		}
		expected := TOTPCode(secret, step)
		if subtle.ConstantTimeCompare([]byte(expected), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

func NewChallengeToken() (string, [32]byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", [32]byte{}, err
	}
	token := "hmfa_" + base64.RawURLEncoding.EncodeToString(raw)
	clear(raw)
	return token, sha256.Sum256([]byte(token)), nil
}

func HashChallengeToken(token string) ([32]byte, error) {
	if !strings.HasPrefix(token, "hmfa_") {
		return [32]byte{}, errors.New("invalid MFA challenge")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, "hmfa_"))
	if err != nil || len(raw) != 32 {
		return [32]byte{}, errors.New("invalid MFA challenge")
	}
	clear(raw)
	return sha256.Sum256([]byte(token)), nil
}

func NewRecoveryCode() (display string, hash [32]byte, err error) {
	raw := make([]byte, 16)
	if _, err = rand.Read(raw); err != nil {
		return "", hash, err
	}
	encoded := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))
	clear(raw)
	display = encoded[:5] + "-" + encoded[5:10] + "-" + encoded[10:15] + "-" + encoded[15:20] + "-" + encoded[20:]
	hash = RecoveryCodeHash(display)
	return display, hash, nil
}

func RecoveryCodeHash(code string) [32]byte {
	normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(code), "-", ""))
	return sha256.Sum256([]byte("heimdall:admin-mfa-recovery:v1:" + normalized))
}
