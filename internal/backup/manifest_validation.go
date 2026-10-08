package backup

import (
	"errors"
	"fmt"

	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func validateManifestHeader(manifest Manifest) error {
	if manifest.Metadata.SchemaVersion > boltstore.CurrentSchemaVersion() {
		return fmt.Errorf(
			"backup metadata schema version %d is newer than this Halro binary supports (schema version %d)",
			manifest.Metadata.SchemaVersion, boltstore.CurrentSchemaVersion(),
		)
	}
	if (manifest.FormatVersion < 1 || manifest.FormatVersion > manifestVersion) || !manifest.Encrypted ||
		manifest.BackupID == "" || manifest.CreatedAt.IsZero() ||
		manifest.Metadata.SchemaVersion == 0 ||
		manifest.Metadata.TxID == 0 ||
		manifest.CheckpointWatermark.After(manifest.LedgerWatermark) ||
		!validFingerprint(manifest.MasterKeyFingerprint) {
		return errors.New("backup manifest is invalid")
	}
	if manifest.FormatVersion >= 2 && (manifest.LedgerFeatureEpoch < 4 ||
		manifest.MinimumLedgerReaderVersion != fmt.Sprintf("v%d", manifest.LedgerFeatureEpoch) ||
		!validSHA256Label(manifest.PricingStateSHA256) || !validSHA256Label(manifest.PendingIntentSHA256)) {
		return errors.New("backup compatibility manifest is invalid")
	}
	if manifest.FormatVersion >= 3 && (manifest.GovernanceFormatVersion != 1 || manifest.GovernanceOffset < 0) {
		return errors.New("backup Governance manifest is invalid")
	}
	if manifest.FormatVersion >= 4 {
		metadataHead, metadataOK := manifest.PerStoreHead["metadata"]
		if manifest.ClusterID == "" || manifest.ClusterIncarnation == "" || manifest.SourceNodeID == "" ||
			manifest.SourceRole != "replica" || manifest.Term == 0 || len(manifest.PerStoreHead) != 4 ||
			!metadataOK || metadataHead.Generation != manifest.MetadataJournalEpoch || metadataHead.Sequence != manifest.MetadataJournalSequence {
			return errors.New("backup Replica prefix manifest is invalid")
		}
		for _, name := range []string{"ledger", "audit", "governance", "metadata"} {
			if _, ok := manifest.PerStoreHead[name]; !ok {
				return errors.New("backup Replica per-store head set is incomplete")
			}
		}
	}
	if manifest.LedgerChainVerified && (manifest.LedgerChainHeadSequence > manifest.LedgerWatermark.Sequence ||
		manifest.LedgerChainHeadOffset > manifest.LedgerWatermark.Offset) {
		return errors.New("backup manifest ledger chain head is inconsistent with the ledger watermark")
	}
	if manifest.KeySlotDescriptorSHA256 != "" && !validSHA256(manifest.KeySlotDescriptorSHA256) {
		return errors.New("backup manifest Key Slot descriptor digest is invalid")
	}
	return nil
}
