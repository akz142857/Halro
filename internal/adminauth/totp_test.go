package adminauth

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func TestTOTPUsesRFC6238SHA1Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for _, test := range []struct {
		unix   int64
		prefix string
	}{{59, "287082"}, {1111111109, "081804"}, {1234567890, "005924"}, {2000000000, "279037"}} {
		code := TOTPCode(secret, test.unix/TOTPPeriod)
		if code != test.prefix {
			t.Fatalf("unix=%d code=%s want=%s", test.unix, code, test.prefix)
		}
	}
}

func TestVerifyTOTPWindowAndReplay(t *testing.T) {
	secret, _ := hex.DecodeString("3132333435363738393031323334353637383930")
	now := time.Unix(1234567890, 0)
	current := now.Unix() / TOTPPeriod
	for _, step := range []int64{current - 1, current, current + 1} {
		accepted, ok := VerifyTOTP(secret, TOTPCode(secret, step), now, 0)
		if !ok || accepted != step {
			t.Fatalf("step %d rejected", step)
		}
	}
	if _, ok := VerifyTOTP(secret, TOTPCode(secret, current), now, current); ok {
		t.Fatal("replayed step accepted")
	}
	if _, ok := VerifyTOTP(secret, "12345x", now, 0); ok {
		t.Fatal("non-numeric code accepted")
	}
}

func TestChallengeAndRecoveryTokens(t *testing.T) {
	token, hash, err := NewChallengeToken()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := HashChallengeToken(token)
	if err != nil || parsed != hash {
		t.Fatal("challenge did not round trip")
	}
	display, recoveryHash, err := NewRecoveryCode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(display, "-") != 4 || RecoveryCodeHash(strings.ToUpper(display)) != recoveryHash {
		t.Fatal("recovery code normalization failed")
	}
}
