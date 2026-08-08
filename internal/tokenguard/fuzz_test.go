package tokenguard

import (
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

func FuzzRestoreCheckpointNeverPanics(f *testing.F) {
	f.Add([]byte(`{"version":1,"subjects":[]}`))
	f.Add([]byte(`{"version":999,"subjects":[]}`))
	f.Add([]byte(`not-json`))
	f.Fuzz(func(t *testing.T, payload []byte) {
		manager, err := New([]domain.TokenGuardPolicy{{
			ID: "guard", Name: "Guard", Enabled: true, Action: "observe",
			EWMAEnabled: true, EWMAAlpha: 0.2, EWMAMultiplier: 3,
			EWMAMinimumSamples: 10, EWMAWarmup: time.Minute,
			EWMAEvaluationWindow: time.Minute, EWMACooldown: time.Minute,
			EWMAAbsoluteRPM: 10,
		}})
		if err != nil {
			t.Fatal(err)
		}
		_ = manager.RestoreCheckpoint(payload)
	})
}
