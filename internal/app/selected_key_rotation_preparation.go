package app

import (
	"context"
	"errors"
	"math/bits"
	"os"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// Incomplete candidate namespaces have no authority and no guessed creator
// receipts. They remain opaque, intact and fully charged; a retry exclusively
// creates another member of this finite namespace. Slot/attempt exhaustion
// refuses before a provider wrap or a new intent publication.
func selectRotationPreparationSlot(ctx context.Context, cfg config.Config, source memoryStateBootstrap, opaque uint16) (uint8, error) {
	if opaque>>selectedRotationMetadataSlots != 0 || source.MetadataSlot >= selectedRotationMetadataSlots {
		return 0, resourcebudget.ErrInvalid
	}
	m := keyScope(ctx).root.pressure.logicalBudget()
	count := int64(bits.OnesCount16(opaque))
	if count != 0 {
		if _, err := m.TryAcquireRecovery(resourcebudget.Cost{Disk: count * (cfg.ResourceBudget.MetadataStoreDiskBytes + cfg.ResourceBudget.MetadataJournalDiskBytes)}); err != nil {
			return 0, err
		}
	}
	for slot := uint8(0); slot < selectedRotationMetadataSlots; slot++ {
		if slot == source.MetadataSlot || opaque&(uint16(1)<<slot) != 0 {
			continue
		}
		root, journal := memoryStateMetadataPaths(cfg, slot)
		_, re := os.Lstat(root)
		_, je := os.Lstat(journal)
		if errors.Is(re, os.ErrNotExist) && errors.Is(je, os.ErrNotExist) {
			return slot, nil
		}
		if re != nil && !errors.Is(re, os.ErrNotExist) {
			return 0, re
		}
		if je != nil && !errors.Is(je, os.ErrNotExist) {
			return 0, je
		}
		// A preexisting unqualified namespace is preserved. It cannot select a
		// root or supply candidate content to this operation.
	}
	return 0, errors.New("eight Metadata namespaces are occupied; explicit offline archival is required before selected DEK rotation")
}
