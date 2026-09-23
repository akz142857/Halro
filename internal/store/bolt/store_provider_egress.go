package bolt

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/akz142857/Halro/internal/domain"
)

// PutProviderEgressProxy commits a proxy definition and its optional internal
// credential as one mutation. deleteCredentialID is used when Basic auth is
// explicitly removed; it is never inferred from an empty input.
func (s *Store) PutProviderEgressProxy(
	ctx context.Context,
	proxy domain.ProviderEgressProxy,
	expectedRevision uint64,
	authCredential *domain.Credential,
	expectedCredentialRevision uint64,
	deleteCredentialID string,
	intent *domain.AdminAuditIntent,
) (domain.ProviderEgressProxy, error) {
	if err := proxy.Validate(); err != nil {
		return domain.ProviderEgressProxy{}, err
	}
	if authCredential != nil {
		if proxy.BasicAuthCredentialID == "" || proxy.BasicAuthCredentialID != authCredential.ID {
			return domain.ProviderEgressProxy{}, errors.New("Provider egress proxy authentication reference is invalid")
		}
		if err := authCredential.Validate(); err != nil {
			return domain.ProviderEgressProxy{}, err
		}
		if authCredential.Type != domain.ProviderEgressCredentialType || authCredential.Audience != proxy.Endpoint {
			return domain.ProviderEgressProxy{}, errors.New("Provider egress proxy authentication binding is invalid")
		}
	}
	if deleteCredentialID != "" && proxy.BasicAuthCredentialID != "" {
		return domain.ProviderEgressProxy{}, errors.New("cannot delete active Provider egress authentication")
	}
	if err := ctx.Err(); err != nil {
		return domain.ProviderEgressProxy{}, err
	}
	err := s.update(func(tx *Tx) error {
		proxies := tx.Bucket(bucketProviderEgressProxies)
		var current domain.ProviderEgressProxy
		if raw := proxies.Get([]byte(proxy.ID)); raw != nil {
			if err := json.Unmarshal(raw, &current); err != nil {
				return err
			}
		}
		if expectedRevision == 0 && proxies.Get([]byte(proxy.ID)) == nil && proxies.Stats().KeyN >= domain.MaxProviderEgressProxies {
			return ErrProviderEgressProxyLimit
		}
		if deleteCredentialID != "" && current.BasicAuthCredentialID != deleteCredentialID {
			return errors.New("Provider egress proxy cannot delete authentication it does not own")
		}
		if authCredential != nil {
			if err := putVersioned(tx.Bucket(bucketCredentials), authCredential.ID, expectedCredentialRevision, authCredential); err != nil {
				return err
			}
		}
		if proxy.BasicAuthCredentialID != "" {
			rawCredential := tx.Bucket(bucketCredentials).Get([]byte(proxy.BasicAuthCredentialID))
			if rawCredential == nil {
				return errors.New("Provider egress proxy authentication is unavailable")
			}
			var credential domain.Credential
			if err := json.Unmarshal(rawCredential, &credential); err != nil {
				return err
			}
			if credential.Type != domain.ProviderEgressCredentialType || credential.Audience != proxy.Endpoint {
				return errors.New("Provider egress proxy authentication binding is invalid")
			}
			if err := proxies.ForEach(func(key, value []byte) error {
				if value == nil || string(key) == proxy.ID {
					return nil
				}
				var other domain.ProviderEgressProxy
				if err := json.Unmarshal(value, &other); err != nil {
					return err
				}
				if other.BasicAuthCredentialID == proxy.BasicAuthCredentialID {
					return errors.New("Provider egress proxy authentication is already owned")
				}
				return nil
			}); err != nil {
				return err
			}
		}
		if deleteCredentialID != "" {
			if err := tx.Bucket(bucketCredentials).Delete([]byte(deleteCredentialID)); err != nil {
				return err
			}
		}
		if err := putVersioned(proxies, proxy.ID, expectedRevision, &proxy); err != nil {
			return err
		}
		return putAdminAuditIntentTx(tx, intent)
	})
	return proxy, err
}

func (s *Store) GetProviderEgressProxy(ctx context.Context, id string) (domain.ProviderEgressProxy, error) {
	var proxy domain.ProviderEgressProxy
	err := s.getJSON(ctx, bucketProviderEgressProxies, id, &proxy)
	return proxy, err
}

func (s *Store) ListProviderEgressProxies(ctx context.Context) ([]domain.ProviderEgressProxy, error) {
	var proxies []domain.ProviderEgressProxy
	err := s.listJSON(ctx, bucketProviderEgressProxies, func(raw []byte) error {
		var proxy domain.ProviderEgressProxy
		if err := json.Unmarshal(raw, &proxy); err != nil {
			return err
		}
		proxies = append(proxies, proxy)
		return nil
	})
	sort.Slice(proxies, func(i, j int) bool { return proxies[i].ID < proxies[j].ID })
	return proxies, err
}

func (s *Store) DeleteProviderEgressProxy(ctx context.Context, id string, expectedRevision uint64, intent *domain.AdminAuditIntent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id == "" || expectedRevision == 0 {
		return errors.New("Provider egress proxy id and expected revision are required")
	}
	return s.update(func(tx *Tx) error {
		proxies := tx.Bucket(bucketProviderEgressProxies)
		raw := proxies.Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		var proxy domain.ProviderEgressProxy
		if err := json.Unmarshal(raw, &proxy); err != nil {
			return err
		}
		if proxy.Revision != expectedRevision {
			return ErrRevisionConflict
		}
		providers := tx.Bucket(bucketProviders)
		if err := providers.ForEach(func(_, value []byte) error {
			var instance domain.ProviderInstance
			if err := json.Unmarshal(value, &instance); err != nil {
				return err
			}
			if instance.DeletedAt == nil && instance.EgressProxyID == id {
				return ErrProviderEgressProxyInUse
			}
			return nil
		}); err != nil {
			return err
		}
		if proxy.BasicAuthCredentialID != "" {
			if err := tx.Bucket(bucketCredentials).Delete([]byte(proxy.BasicAuthCredentialID)); err != nil {
				return err
			}
		}
		if err := proxies.Delete([]byte(id)); err != nil {
			return err
		}
		return putAdminAuditIntentTx(tx, intent)
	})
}
