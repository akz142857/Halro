package adminauth

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/akz142857/Heimdall/internal/domain"
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
)

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
	hash := argon2.IDKey(password, salt, argonIterations, argonMemoryKiB, argonParallelism, passwordHashSize)
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

func VerifyPassword(user domain.AdminUser, password []byte) bool {
	if user.PasswordVersion != passwordVersion || len(password) == 0 ||
		len(user.PasswordSalt) < passwordSaltSize || len(user.PasswordHash) != passwordHashSize {
		return false
	}
	candidate := argon2.IDKey(
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
	salt := []byte("heimdall-dummy!!")
	candidate := argon2.IDKey(
		password, salt, argonIterations, argonMemoryKiB, argonParallelism, passwordHashSize,
	)
	clear(candidate)
}
