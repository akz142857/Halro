package adminauth

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/akz142857/Halro/internal/domain"
	"golang.org/x/crypto/argon2"
)

const (
	passwordVersion  = 1
	argonMemoryKiB   = 64 * 1024
	argonIterations  = 3
	argonParallelism = 2
	passwordSaltSize = 16
	passwordHashSize = 32
	minPasswordChars = 8
	maxPasswordBytes = 1024
	// Each Argon2 invocation allocates argonMemoryKiB (64 MiB). Keep the
	// process-wide working set bounded across login, step-up, dummy verification,
	// and password creation; callers wait for a slot instead of allocating an
	// unbounded number of blocks under a multi-source login storm.
	argonHashConcurrency = 2
)

var argonHashSlots = make(chan struct{}, argonHashConcurrency)

func derivePasswordKey(password, salt []byte, iterations, memoryKiB uint32, parallelism uint8, size uint32) []byte {
	argonHashSlots <- struct{}{}
	defer func() { <-argonHashSlots }()
	return argon2.IDKey(password, salt, iterations, memoryKiB, parallelism, size)
}

func NewUser(username string, password []byte, role string, now time.Time) (domain.AdminUser, error) {
	if !utf8.Valid(password) {
		return domain.AdminUser{}, errors.New("admin password must be valid UTF-8")
	}
	if utf8.RuneCount(password) < minPasswordChars {
		return domain.AdminUser{}, errors.New("admin password must contain at least 8 characters")
	}
	if len(password) > maxPasswordBytes {
		return domain.AdminUser{}, errors.New("admin password is too long")
	}
	salt := make([]byte, passwordSaltSize)
	if _, err := rand.Read(salt); err != nil {
		return domain.AdminUser{}, err
	}
	hash := derivePasswordKey(password, salt, argonIterations, argonMemoryKiB, argonParallelism, passwordHashSize)
	user := domain.AdminUser{
		Username: username, Role: role, Appearance: domain.AppearanceDark, PasswordVersion: passwordVersion,
		PasswordSalt: salt, PasswordHash: hash, ArgonMemoryKiB: argonMemoryKiB,
		ArgonIterations: argonIterations, ArgonParallelism: argonParallelism,
		SessionGeneration: 1, CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	}
	if err := user.Validate(); err != nil {
		clear(hash)
		clear(salt)
		return domain.AdminUser{}, err
	}
	return user, nil
}

// VerifyPassword reports whether password matches the user's stored hash.
//
// The []byte parameter is argon2's shape, not a scrubbing boundary, and the
// callers deliberately do not treat it as one. An admin password arrives in a
// JSON body: by the time a handler sees it the plaintext is in net/http's read
// buffer, in the decoder's buffer, and in the immutable string the decoder
// produced, none of which the handler owns or can overwrite. Copying that
// string into a []byte so the copy could be clear()ed — which several handlers
// used to do — zeroed the one instance that was about to become garbage while
// the unreachable ones stayed exactly as long, so it shortened nothing and
// read as though the secret had been scrubbed from the process. What bounds
// this material is that it is never logged, persisted, or echoed.
//
// The candidate hash below is the opposite case: this function derives it, so
// it is the only holder and clearing it is real.
func VerifyPassword(user domain.AdminUser, password []byte) bool {
	if user.PasswordVersion != passwordVersion || len(password) == 0 ||
		len(user.PasswordSalt) < passwordSaltSize || len(user.PasswordHash) != passwordHashSize {
		return false
	}
	candidate := derivePasswordKey(
		password,
		user.PasswordSalt,
		user.ArgonIterations,
		user.ArgonMemoryKiB,
		user.ArgonParallelism,
		uint32(len(user.PasswordHash)),
	)
	defer clear(candidate)
	return subtle.ConstantTimeCompare(candidate, user.PasswordHash) == 1
}

func PasswordNeedsUpgrade(user domain.AdminUser) bool {
	return user.PasswordVersion != passwordVersion ||
		user.ArgonMemoryKiB != argonMemoryKiB ||
		user.ArgonIterations != argonIterations ||
		user.ArgonParallelism != argonParallelism ||
		len(user.PasswordHash) != passwordHashSize
}

func DummyVerify(password []byte) {
	salt := []byte("halro-dummy!!")
	candidate := derivePasswordKey(
		password, salt, argonIterations, argonMemoryKiB, argonParallelism, passwordHashSize,
	)
	clear(candidate)
}
