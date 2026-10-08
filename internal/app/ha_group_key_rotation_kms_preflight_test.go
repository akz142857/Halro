package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	corekms "github.com/akz142857/Halro/internal/kms"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func groupKMSWrapCalls(h *kmsAppHarness) int {
	n := 0
	for _, w := range h.wrappers {
		for _, c := range w.Calls() {
			if c.Operation == corekms.OperationWrap {
				n++
			}
		}
	}
	return n
}
func TestHAGroupKMSRotationPreflightBudgetAndForeignCarrierBeforeWrap(t *testing.T) {
	for _, failure := range []string{"budget", "signed-foreign-instance"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			fixture, owners, h := prepareHAGroupKMSColdMembers(t, ctx)
			before := groupKMSWrapCalls(h)
			var held resourcebudget.Permit
			if failure == "budget" {
				m := owners[0].owners.Components.Budget
				limits := fixture.configs[0].ResourceBudget.Limits(time.Minute)
				stats := m.Snapshot()
				remaining := min(limits.Total.Disk-stats.Used.Disk, limits.RecoveryReserve.Disk-stats.Recovery.Disk, limits.Classes[resourcebudget.Recovery].Disk-stats.Classes[resourcebudget.Recovery].Disk)
				if remaining <= 0 {
					t.Fatal("fixture requires a positive actual Disk remainder")
				}
				var err error
				held, err = m.TryAcquireRecovery(resourcebudget.Cost{Disk: remaining})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				cfg := fixture.configs[0]
				path := haMemoryUnlockPath(cfg)
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				p, envelope, err := decodeHAMemoryUnlock(raw)
				if err != nil {
					t.Fatal(err)
				}
				p.InstanceID += "-foreign"
				envelope.Payload, err = json.Marshal(p)
				if err != nil {
					t.Fatal(err)
				}
				envelope.MAC, err = haMemoryUnlockMAC(fixture.keys.Master, envelope.Payload)
				if err != nil {
					t.Fatal(err)
				}
				forged, err := json.Marshal(envelope)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, forged, 0600); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					actual, err := os.ReadFile(path)
					if err != nil || !bytes.Equal(actual, forged) {
						t.Error("foreign carrier was removed/replaced", err)
					}
				})
			}
			options := kmsRotationOptions{operationID: "preflight-failure-group", factory: h.factory, now: time.Now}
			r, err := prepareHAGroupRotationWithKMS(ctx, owners, fixture.keys.Master, bytes.Repeat([]byte{114}, 32), options.operationID, &options)
			if err == nil {
				t.Fatal("invalid preflight accepted")
			}
			if failure == "signed-foreign-instance" && !errors.Is(err, authenticatedstate.ErrSealIneligible) {
				t.Fatal(err)
			}
			if groupKMSWrapCalls(h) != before {
				t.Fatal("target KMS wrapping happened before complete source/budget eligibility")
			}
			for _, cfg := range fixture.configs {
				for slot := range haGroupRotationIntentSlots {
					if _, err := os.Lstat(haGroupRotationIntentPath(cfg.Storage.DataDir, slot)); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("preflight wrote intent", err)
					}
				}
			}
			if r != nil {
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if held != (resourcebudget.Permit{}) {
				if err := held.Release(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
