package bolt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

// PortableConfiguration is internal to the store and control plane. The Admin
// API projects these records into a separate, allowlisted file format.
type PortableConfiguration struct {
	Providers          []domain.ProviderInstance
	Deployments        []domain.Deployment
	Routes             []domain.Route
	RedactionPolicies  []domain.RedactionPolicy
	TokenGuardPolicies []domain.TokenGuardPolicy
	Projects           []domain.Project
	GatewayKeys        []domain.GatewayKey
	Prices             map[string]domain.DeploymentPriceVersion
}

func (s *Store) ReadPortableConfiguration(ctx context.Context, at time.Time) (PortableConfiguration, error) {
	var config PortableConfiguration
	err := s.view(func(tx *Tx) error {
		for _, load := range []func() error{
			func() error { return readConfigurationRecords(ctx, tx, bucketProviders, &config.Providers) },
			func() error { return readConfigurationRecords(ctx, tx, bucketDeployments, &config.Deployments) },
			func() error { return readConfigurationRecords(ctx, tx, bucketRoutes, &config.Routes) },
			func() error {
				return readConfigurationRecords(ctx, tx, bucketRedactionPolicies, &config.RedactionPolicies)
			},
			func() error {
				return readConfigurationRecords(ctx, tx, bucketTokenGuardPolicies, &config.TokenGuardPolicies)
			},
			func() error { return readConfigurationRecords(ctx, tx, bucketProjects, &config.Projects) },
			func() error { return readConfigurationRecords(ctx, tx, bucketGatewayKeys, &config.GatewayKeys) },
		} {
			if err := load(); err != nil {
				return err
			}
		}
		for i := range config.Providers {
			normalizeProviderBindings(&config.Providers[i])
		}
		config.Prices = make(map[string]domain.DeploymentPriceVersion)
		for _, deployment := range config.Deployments {
			if deployment.DeletedAt != nil || deployment.PricingQuarantined {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			timeline, err := listDeploymentPriceVersionsTx(tx, deployment.ID)
			if err != nil {
				return err
			}
			price, err := domain.SelectDeploymentPriceVersion(timeline, deployment.ID, at)
			if errors.Is(err, domain.ErrPriceUnavailable) {
				continue
			}
			if err != nil {
				return err
			}
			config.Prices[deployment.ID] = price
		}
		return nil
	})
	return config, err
}

func readConfigurationRecords[T any](ctx context.Context, tx *Tx, bucket []byte, result *[]T) error {
	return tx.Bucket(bucket).ForEach(func(_, raw []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var item T
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
		*result = append(*result, item)
		return nil
	})
}

var ErrConfigurationTargetNotEmpty = errors.New("target already has portable configuration")

func (s *Store) CheckConfigurationImportTarget() error {
	return s.view(checkConfigurationImportTarget)
}

func checkConfigurationImportTarget(tx *Tx) error {
	for _, bucket := range [][]byte{
		bucketProviders, bucketDeployments, bucketRoutes, bucketRedactionPolicies,
		bucketTokenGuardPolicies, bucketProjects, bucketGatewayKeys,
	} {
		if key, _ := tx.Bucket(bucket).Cursor().First(); key != nil {
			return fmt.Errorf("%w: %s", ErrConfigurationTargetNotEmpty, bucket)
		}
	}
	return nil
}

// ImportPortableConfiguration commits prepared configuration and its audit
// intent in one recorded transaction. Existing credentials and egress proxies
// are references to the target instance and are never written here.
func (s *Store) ImportPortableConfiguration(ctx context.Context, config PortableConfiguration, intent *domain.AdminAuditIntent) error {
	if err := s.updateContext(ctx, func(tx *Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := checkConfigurationImportTarget(tx); err != nil {
			return err
		}
		for _, write := range []func() error{
			func() error {
				return writeConfigurationRecords(tx, bucketProviders, config.Providers, func(v domain.ProviderInstance) string { return v.ID })
			},
			func() error {
				return writeConfigurationRecords(tx, bucketDeployments, config.Deployments, func(v domain.Deployment) string { return v.ID })
			},
			func() error {
				return writeConfigurationRecords(tx, bucketRoutes, config.Routes, func(v domain.Route) string { return v.ID })
			},
			func() error {
				return writeConfigurationRecords(tx, bucketRedactionPolicies, config.RedactionPolicies, func(v domain.RedactionPolicy) string { return v.ID })
			},
			func() error {
				return writeConfigurationRecords(tx, bucketTokenGuardPolicies, config.TokenGuardPolicies, func(v domain.TokenGuardPolicy) string { return v.ID })
			},
			func() error {
				return writeConfigurationRecords(tx, bucketProjects, config.Projects, func(v domain.Project) string { return v.ID })
			},
		} {
			if err := write(); err != nil {
				return err
			}
		}
		for _, key := range config.GatewayKeys {
			index := tx.Bucket(bucketGatewayKeyHash)
			if index.Get(key.KeyHash[:]) != nil {
				return ErrKeyHashConflict
			}
			if err := putVersioned(tx.Bucket(bucketGatewayKeys), key.ID, 0, &key); err != nil {
				return err
			}
			if err := index.Put(key.KeyHash[:], []byte(key.ID)); err != nil {
				return err
			}
		}
		for _, deployment := range config.Deployments {
			price, ok := config.Prices[deployment.ID]
			if !ok {
				continue
			}
			if err := putDeploymentPriceVersionTx(tx, price); err != nil {
				return err
			}
			if err := tx.Bucket(bucketDeploymentPriceNext).Put([]byte(deployment.ID), encodeUint64(price.Version)); err != nil {
				return err
			}
		}
		return putAdminAuditIntentTx(tx, intent)
	}); err != nil {
		return err
	}
	for deploymentID := range config.Prices {
		s.invalidateDeploymentPricingTimeline(deploymentID)
	}
	return nil
}

func writeConfigurationRecords[T any, P interface {
	*T
	revisioned
}](tx *Tx, bucket []byte, records []T, recordID func(T) string) error {
	for i := range records {
		if err := putVersioned(tx.Bucket(bucket), recordID(records[i]), 0, P(&records[i])); err != nil {
			return err
		}
	}
	return nil
}
