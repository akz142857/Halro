package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
)

// StoredRouteSuspension is one durable suspension, as the offline command shows
// it. The scope handle is what `halro route clear-suspension` takes back.
type StoredRouteSuspension struct {
	ScopeID            string `json:"scope_id"`
	ScopeKind          string `json:"scope_kind"`
	ScopeKey           string `json:"scope_key"`
	Reason             string `json:"reason"`
	ProviderStatus     int    `json:"provider_status,omitempty"`
	ProviderCode       string `json:"provider_code,omitempty"`
	ObservedAt         string `json:"observed_at"`
	Until              string `json:"until,omitempty"`
	Indefinite         bool   `json:"indefinite"`
	CredentialRevision uint64 `json:"credential_revision,omitempty"`
}

// ListStoredRouteSuspensions reads what would be restored at the next start.
//
// It is the durable set, not the live one, and the difference is the point: a
// running instance holds short availability windows this never sees, and the
// Admin API is where to look at those. Here the data lock means the instance is
// stopped, so the durable set is the whole of what the gate will believe.
func ListStoredRouteSuspensions(ctx context.Context, cfg config.Config) ([]StoredRouteSuspension, error) {
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return nil, err
	}
	defer dataLock.Close()
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return nil, err
	}
	defer store.Close()
	stored, err := store.ListRouteSuspensions(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]StoredRouteSuspension, 0, len(stored))
	for _, suspension := range stored {
		item := StoredRouteSuspension{
			ScopeID:            suspension.ScopeID(),
			ScopeKind:          suspension.ScopeKind,
			ScopeKey:           readableRouteScopeKey(suspension.ScopeKey),
			Reason:             suspension.Reason,
			ProviderStatus:     suspension.ProviderStatus,
			ProviderCode:       suspension.ProviderCode,
			ObservedAt:         suspension.ObservedAt.UTC().Format(time.RFC3339),
			Indefinite:         suspension.Indefinite,
			CredentialRevision: suspension.CredentialRevision,
		}
		if !suspension.Indefinite && !suspension.Until.IsZero() {
			item.Until = suspension.Until.UTC().Format(time.RFC3339)
		}
		items = append(items, item)
	}
	return items, nil
}

// ClearStoredRouteSuspension removes one durable suspension and records the
// action in the audit chain.
//
// Offline, so it clears what the gate would be restored from rather than what a
// running gate holds — the data lock makes those the same thing. An operator
// clearing one while Halro is up uses the Admin API, which clears both halves.
func ClearStoredRouteSuspension(ctx context.Context, cfg config.Config, scopeID string) error {
	kind, key, ok := domain.DecodeRouteScopeID(scopeID)
	if !ok {
		return errors.New("scope id is not a suspension handle from the listing")
	}
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return err
	}
	defer dataLock.Close()
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return err
	}
	defer store.Close()
	// nil intent: this is an offline command holding the data lock, and it
	// appends its own audit record below.
	if err := store.DeleteRouteSuspensionWithAuditIntent(ctx, kind, key, nil); err != nil {
		if errors.Is(err, boltstore.ErrNotFound) {
			return fmt.Errorf("no stored suspension for %s", scopeID)
		}
		return err
	}
	return appendOfflineAudit(ctx, cfg, store, "route_suspension.clear", "route_suspension", scopeID)
}

// readableRouteScopeKey renders a scope key for a human. The
// credential-and-model scope joins its halves with a NUL so they cannot collide
// inside the gate; that byte has no business in JSON.
func readableRouteScopeKey(key string) string {
	runes := []rune(key)
	for index, symbol := range runes {
		if symbol == 0 {
			runes[index] = '/'
		}
	}
	return string(runes)
}
