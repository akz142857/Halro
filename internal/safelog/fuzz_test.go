package safelog

import (
	"bytes"
	"log/slog"
	"testing"
)

// FuzzRedactNeverLeaksSeededSecret drives a credential through every attribute
// shape slog can carry, wrapped in fuzzer-chosen surroundings. The shape tests
// beside this one enumerate the cases a reader can think of; this is for the
// ones nobody thought of — a credential concatenated with a prefix that breaks
// a pattern anchor, a name that only nearly matches a sensitive key, a value
// long enough to be split by a handler.
//
// The canary is always a recognised format, so a leak means redaction failed
// rather than that the fuzzer invented an unknown credential shape.
func FuzzRedactNeverLeaksSeededSecret(f *testing.F) {
	for _, seed := range []struct {
		key         string
		prefix      string
		suffix      string
		shape       uint8
		asAttribute bool
	}{
		{"authorization", "Bearer ", "", 0, true},
		{"provider", "", " trailing", 1, true},
		{"detail", "prefix ", "", 2, false},
		{"api_key", "", "", 3, true},
		{"nested", "{\"token\":\"", "\"}", 4, true},
		{"message", "connect failed: ", " (retrying)", 5, false},
		{"Authorization", "\n", "\t", 6, true},
	} {
		f.Add(seed.key, seed.prefix, seed.suffix, seed.shape, seed.asAttribute)
	}
	f.Fuzz(func(t *testing.T, key, prefix, suffix string, shape uint8, asAttribute bool) {
		if len(key)+len(prefix)+len(suffix) > 4<<10 {
			t.Skip()
		}
		secret := prefix + canary + suffix
		var output bytes.Buffer
		logger := New(slog.NewJSONHandler(&output, nil))
		if !asAttribute {
			logger.Info(secret)
		} else {
			switch shape % 7 {
			case 0:
				logger.Info("event", key, secret)
			case 1:
				logger.Info("event", slog.Group("group", slog.String(key, secret)))
			case 2:
				logger.Info("event", slog.Any(key, credentialValuer{secret: secret}))
			case 3:
				logger.Info("event", slog.Any(key, credentialStringer{secret: secret}))
			case 4:
				logger.Info("event", slog.Any(key, providerConfig{Endpoint: "https://api.example.com", APIKey: secret}))
			case 5:
				logger.With(key, secret).Info("event")
			case 6:
				logger.WithGroup("outer").Info("event", key, secret)
			}
		}
		if leaked(output.String()) {
			t.Fatalf("credential reached the handler: key=%q shape=%d attribute=%v output=%s",
				key, shape%7, asAttribute, output.String())
		}
	})
}
