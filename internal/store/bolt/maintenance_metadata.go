package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/akz142857/Halro/internal/domain"
)

// These structural ceilings precede domain allocation. One valid native wide
// row may use up to 8 MiB, but cannot manufacture millions of decoded objects.
func decodeMetadataMaintenanceJSON(ctx context.Context, raw []byte, target any) error {
	if ctx == nil {
		return ErrPricingRecoveryLimit
	}
	if len(raw) == 0 || len(raw) > pricingRecoveryWideRowBytes {
		return ErrPricingRecoveryLimit
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	depth, nodes, roots := 0, 0, 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		token, err := d.Token()
		if errors.Is(err, io.EOF) {
			if depth != 0 || roots != 1 {
				return ErrPricingRecoveryLimit
			}
			break
		}
		if err != nil {
			return err
		}
		nodes++
		if nodes > 4096 {
			return ErrPricingRecoveryLimit
		}
		if value, ok := token.(json.Delim); ok {
			switch value {
			case '{', '[':
				if depth == 0 {
					roots++
					if value != '{' {
						return ErrPricingRecoveryLimit
					}
				}
				depth++
				if depth > 32 {
					return ErrPricingRecoveryLimit
				}
			case '}', ']':
				depth--
			}
		} else if depth == 0 {
			return ErrPricingRecoveryLimit
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return err
	}
	return ctx.Err()
}

func (s *Store) getMetadataMaintenance(ctx context.Context, bucket []byte, id string, target any) error {
	if ctx == nil || len(id) == 0 || len(id) > 4096 {
		return ErrPricingRecoveryLimit
	}
	var raw []byte
	err := s.view(func(tx *Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		b := tx.Bucket(bucket)
		if b == nil {
			return ErrNotFound
		}
		value := b.Get([]byte(id))
		if value == nil {
			return ErrNotFound
		}
		if len(value) > pricingRecoveryWideRowBytes-len(id) {
			return ErrPricingRecoveryLimit
		}
		raw = bytes.Clone(value)
		return nil
	})
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return decodeMetadataMaintenanceJSON(ctx, raw, target)
}

func walkMetadataMaintenance[T any](s *Store, ctx context.Context, bucket []byte, identity func(T) string, consume func(T) error) error {
	if ctx == nil || consume == nil {
		return ErrPricingRecoveryLimit
	}
	return s.walkVaultMaintenance(ctx, bucket, func(key, raw []byte) error {
		var item T
		if len(key) > 4096 {
			return ErrPricingRecoveryLimit
		}
		if err := decodeMetadataMaintenanceJSON(ctx, raw, &item); err != nil {
			return err
		}
		if identity(item) == "" || identity(item) != string(key) {
			return ErrVaultRewriteIncomplete
		}
		return consume(item)
	})
}

func (s *Store) GetProviderMaintenance(ctx context.Context, id string) (result domain.ProviderInstance, err error) {
	err = s.getMetadataMaintenance(ctx, bucketProviders, id, &result)
	if err == nil && result.ID != id {
		err = ErrVaultRewriteIncomplete
	}
	if err != nil {
		return domain.ProviderInstance{}, err
	}
	normalizeProviderBindings(&result)
	return result, nil
}
func (s *Store) GetDeploymentMaintenance(ctx context.Context, id string) (result domain.Deployment, err error) {
	err = s.getMetadataMaintenance(ctx, bucketDeployments, id, &result)
	if err == nil && result.ID != id {
		err = ErrVaultRewriteIncomplete
	}
	if err != nil {
		return domain.Deployment{}, err
	}
	if provider, providerErr := s.GetProviderMaintenance(ctx, result.ProviderID); providerErr == nil {
		normalizeDeploymentBinding(&result, provider)
	} else if !errors.Is(providerErr, ErrNotFound) {
		return domain.Deployment{}, providerErr
	}
	return result, nil
}
func (s *Store) GetCredentialMaintenance(ctx context.Context, id string) (result domain.Credential, err error) {
	err = s.getMetadataMaintenance(ctx, bucketCredentials, id, &result)
	if err == nil && result.ID != id {
		err = ErrVaultRewriteIncomplete
	}
	if err != nil {
		return domain.Credential{}, err
	}
	return result, nil
}
func (s *Store) WalkProvidersMaintenance(ctx context.Context, consume func(domain.ProviderInstance) error) error {
	if consume == nil {
		return ErrPricingRecoveryLimit
	}
	return walkMetadataMaintenance(s, ctx, bucketProviders, func(v domain.ProviderInstance) string { return v.ID }, func(v domain.ProviderInstance) error {
		normalizeProviderBindings(&v)
		return consume(v)
	})
}
func (s *Store) WalkDeploymentsMaintenance(ctx context.Context, consume func(domain.Deployment) error) error {
	if consume == nil {
		return ErrPricingRecoveryLimit
	}
	return walkMetadataMaintenance(s, ctx, bucketDeployments, func(v domain.Deployment) string { return v.ID }, func(v domain.Deployment) error {
		if p, err := s.GetProviderMaintenance(ctx, v.ProviderID); err == nil {
			normalizeDeploymentBinding(&v, p)
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		return consume(v)
	})
}
func (s *Store) WalkAdminUsersMaintenance(ctx context.Context, consume func(domain.AdminUser) error) error {
	return walkMetadataMaintenance(s, ctx, bucketAdminUsers, func(v domain.AdminUser) string { return v.Username }, consume)
}
func (s *Store) WalkRoutesMaintenance(ctx context.Context, consume func(domain.Route) error) error {
	return walkMetadataMaintenance(s, ctx, bucketRoutes, func(v domain.Route) string { return v.ID }, consume)
}
func (s *Store) WalkGatewayKeysMaintenance(ctx context.Context, consume func(domain.GatewayKey) error) error {
	return walkMetadataMaintenance(s, ctx, bucketGatewayKeys, func(v domain.GatewayKey) string { return v.ID }, consume)
}
func (s *Store) WalkProviderEgressProxiesMaintenance(ctx context.Context, consume func(domain.ProviderEgressProxy) error) error {
	return walkMetadataMaintenance(s, ctx, bucketProviderEgressProxies, func(v domain.ProviderEgressProxy) string { return v.ID }, consume)
}
