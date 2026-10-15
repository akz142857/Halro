package bolt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

var bucketPricingMigrationResolutions = []byte("pricing_migration_resolutions")

type PricingMigrationItem struct {
	DeploymentID           string `json:"deployment_id"`
	Enabled                bool   `json:"enabled"`
	Classification         string `json:"classification"`
	InputMicrosPerMillion  int64  `json:"input_micros_per_million"`
	OutputMicrosPerMillion int64  `json:"output_micros_per_million"`
	FixedRequestMicrosUSD  int64  `json:"fixed_request_micros_usd"`
}

type PricingMigrationReport struct {
	SchemaVersion     uint64                 `json:"schema_version"`
	MetadataSHA256    string                 `json:"metadata_sha256"`
	GeneratedAt       time.Time              `json:"generated_at"`
	Deployments       []PricingMigrationItem `json:"deployments"`
	NonZero           int                    `json:"non_zero"`
	Zero              int                    `json:"zero"`
	UnresolvedEnabled int                    `json:"unresolved_enabled"`
	EstimatedDowntime string                 `json:"estimated_downtime"`
	SchemaChanges     []string               `json:"schema_changes"`
}

type PricingMigrationResolution struct {
	Mode                   domain.BillingMode `json:"mode"`
	KeepDisabled           bool               `json:"keep_disabled,omitempty"`
	InputMicrosPerMillion  int64              `json:"input_micros_per_million,omitempty"`
	OutputMicrosPerMillion int64              `json:"output_micros_per_million,omitempty"`
	FixedRequestMicrosUSD  int64              `json:"fixed_request_micros_usd,omitempty"`
	SourceReference        string             `json:"source_reference"`
	SourceContentSHA256    string             `json:"source_content_sha256"`
}

type PricingMigrationResolutionFile struct {
	SchemaVersion  uint64                                `json:"schema_version"`
	MetadataSHA256 string                                `json:"metadata_sha256"`
	Operator       string                                `json:"operator"`
	Resolutions    map[string]PricingMigrationResolution `json:"resolutions"`
}

func DryRunPricingMigration(ctx context.Context, metadataPath string) (PricingMigrationReport, error) {
	if err := ctx.Err(); err != nil {
		return PricingMigrationReport{}, err
	}
	digest, err := fileDigest(metadataPath)
	if err != nil {
		return PricingMigrationReport{}, err
	}
	db, err := bbolt.Open(metadataPath, 0o600, &bbolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		return PricingMigrationReport{}, err
	}
	defer db.Close()
	report := PricingMigrationReport{SchemaVersion: 1, MetadataSHA256: digest, GeneratedAt: time.Now().UTC(), EstimatedDowntime: "one offline metadata publication", SchemaChanges: []string{"deployment price version buckets", "Ledger/Usage/Parquet versioned pricing readers"}}
	err = db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketDeployments)
		if bucket == nil {
			return errors.New("deployment bucket is missing")
		}
		return bucket.ForEach(func(_, raw []byte) error {
			if raw == nil {
				return nil
			}
			var deployment domain.Deployment
			if err := json.Unmarshal(raw, &deployment); err != nil {
				return err
			}
			zero := deployment.InputMicrosPerMillion == 0 && deployment.OutputMicrosPerMillion == 0 && deployment.FixedRequestMicrosUSD == 0
			classification := "metered"
			if zero {
				classification = "unresolved_zero"
				report.Zero++
				if deployment.Enabled && deployment.DeletedAt == nil {
					report.UnresolvedEnabled++
				}
			} else {
				report.NonZero++
			}
			report.Deployments = append(report.Deployments, PricingMigrationItem{deployment.ID, deployment.Enabled, classification, deployment.InputMicrosPerMillion, deployment.OutputMicrosPerMillion, deployment.FixedRequestMicrosUSD})
			return nil
		})
	})
	return report, err
}

// ApplyPricingMigration stages a consistent bbolt snapshot, applies explicit
// zero-price resolutions, runs the normal migration/validation, then publishes
// the metadata atomically while retaining a timestamped rollback copy.
func ApplyPricingMigration(ctx context.Context, metadataPath string, resolutionFile PricingMigrationResolutionFile) (string, error) {
	report, err := DryRunPricingMigration(ctx, metadataPath)
	if err != nil {
		return "", err
	}
	if resolutionFile.SchemaVersion != 1 || resolutionFile.Operator == "" || resolutionFile.MetadataSHA256 != report.MetadataSHA256 {
		return "", errors.New("resolution file does not match the inspected metadata revision")
	}
	for _, item := range report.Deployments {
		if item.Classification == "unresolved_zero" {
			if _, ok := resolutionFile.Resolutions[item.DeploymentID]; !ok {
				return "", fmt.Errorf("deployment %q requires an explicit resolution", item.DeploymentID)
			}
		}
	}
	dir := filepath.Dir(metadataPath)
	temp, err := os.CreateTemp(dir, ".pricing-migrate-*.db")
	if err != nil {
		return "", err
	}
	tempPath := temp.Name()
	temp.Close()
	defer os.Remove(tempPath)
	source, err := bbolt.Open(metadataPath, 0o600, &bbolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		return "", err
	}
	err = source.View(func(tx *bbolt.Tx) error {
		out, err := os.OpenFile(tempPath, os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = tx.WriteTo(out)
		if err == nil {
			err = out.Sync()
		}
		return err
	})
	source.Close()
	if err != nil {
		return "", err
	}
	stage, err := bbolt.Open(tempPath, 0o600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		return "", err
	}
	err = stage.Update(func(tx *bbolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists(bucketPricingMigrationResolutions)
		if err != nil {
			return err
		}
		deployments := tx.Bucket(bucketDeployments)
		for id, resolution := range resolutionFile.Resolutions {
			if !domain.ValidSHA256Label(resolution.SourceContentSHA256) || resolution.SourceReference == "" {
				return fmt.Errorf("resolution %q has incomplete source evidence", id)
			}
			raw := deployments.Get([]byte(id))
			if raw == nil {
				return fmt.Errorf("resolution references unknown deployment %q", id)
			}
			var deployment domain.Deployment
			if err := json.Unmarshal(raw, &deployment); err != nil {
				return err
			}
			switch {
			case resolution.KeepDisabled:
				deployment.Enabled = false
			case resolution.Mode == domain.BillingModeFree: // zero terms are intentional and consumed by the migration marker.
			case resolution.Mode == domain.BillingModeMetered:
				deployment.InputMicrosPerMillion, deployment.OutputMicrosPerMillion, deployment.FixedRequestMicrosUSD = resolution.InputMicrosPerMillion, resolution.OutputMicrosPerMillion, resolution.FixedRequestMicrosUSD
			default:
				return fmt.Errorf("resolution %q has invalid mode", id)
			}
			encoded, _ := json.Marshal(deployment)
			if err := deployments.Put([]byte(id), encoded); err != nil {
				return err
			}
			encoded, _ = json.Marshal(resolution)
			if err := bucket.Put([]byte(id), encoded); err != nil {
				return err
			}
		}
		return nil
	})
	stage.Close()
	if err != nil {
		return "", err
	}
	migrated, err := Open(tempPath)
	if err != nil {
		return "", err
	}
	resolutionJSON, err := json.Marshal(resolutionFile)
	if err != nil {
		migrated.Close()
		return "", err
	}
	resolutionDigest := sha256.Sum256(resolutionJSON)
	effectiveAudit := domain.PricingAuditIntent{EventID: fmt.Sprintf("aud_pricing_migration_%x", resolutionDigest[:12]), OccurredAt: time.Now().UTC(), ActorID: resolutionFile.Operator,
		Action: "deployment_price.migrate", TargetType: "deployment_pricing", TargetID: "pricing_migration", RequestSHA256: "sha256:" + hex.EncodeToString(resolutionDigest[:]),
		ChangeSummary: fmt.Sprintf("before={metadata:%s} after={schema:%d,resolutions:%d}", report.MetadataSHA256, CurrentSchemaVersion(), len(resolutionFile.Resolutions))}
	if err := effectiveAudit.Validate(); err != nil {
		migrated.Close()
		return "", err
	}
	if err := migrated.db.Update(func(tx *bbolt.Tx) error { return putPricingAuditIntentTx(tx, effectiveAudit) }); err != nil {
		migrated.Close()
		return "", err
	}
	if err = migrated.Close(); err != nil {
		return "", err
	}
	backupPath := metadataPath + ".pre-pricing-migration-" + time.Now().UTC().Format("20060102T150405Z")
	if err := os.Rename(metadataPath, backupPath); err != nil {
		return "", err
	}
	if err := os.Rename(tempPath, metadataPath); err != nil {
		_ = os.Rename(backupPath, metadataPath)
		return "", err
	}
	return backupPath, nil
}

// ApplyPricingMigrationDataDir stages and atomically publishes the complete
// data directory so metadata can never be paired with a different WAL/Audit/
// Parquet epoch. The returned path is the full rollback directory.
func ApplyPricingMigrationDataDir(ctx context.Context, dataDir, metadataFile string, resolutionFile PricingMigrationResolutionFile) (string, error) {
	parent := filepath.Dir(dataDir)
	stage, err := os.MkdirTemp(parent, ".pricing-migrate-data-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	if err := copyDirectory(dataDir, stage); err != nil {
		return "", err
	}
	metadataBackup, err := ApplyPricingMigration(ctx, filepath.Join(stage, metadataFile), resolutionFile)
	if err != nil {
		return "", err
	}
	_ = os.Remove(metadataBackup)
	rollback := dataDir + ".pre-pricing-migration-" + time.Now().UTC().Format("20060102T150405Z")
	if err := os.Rename(dataDir, rollback); err != nil {
		return "", err
	}
	if err := os.Rename(stage, dataDir); err != nil {
		_ = os.Rename(rollback, dataDir)
		return "", err
	}
	return rollback, nil
}

func copyDirectory(source, destination string) error {
	return filepath.Walk(source, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if info.IsDir() {
			if relative == "." {
				return nil
			}
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported file in data directory: %s", relative)
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		defer input.Close()
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, input)
		syncErr := output.Sync()
		closeErr := output.Close()
		return errors.Join(copyErr, syncErr, closeErr)
	})
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}
