package awskms

import (
	"errors"
	"regexp"

	corekms "github.com/akz142857/Halro/internal/kms"
)

const (
	contextKeyVersion        = "halro.context_version"
	contextKeyInstanceSHA256 = "halro.instance_sha256"
	contextKeySlotSHA256     = "halro.slot_sha256"
	contextKeyPayloadVersion = "halro.payload_version"
	contextKeyPurpose        = "halro.purpose"
)

var lowercaseSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

func NewBindingContext(binding corekms.PayloadBinding, purpose string) (corekms.BindingContext, error) {
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	if purpose != EncryptionContextPurposePrimary && purpose != EncryptionContextPurposeRecovery {
		return nil, errors.New("AWS KMS Encryption Context purpose is invalid")
	}
	instanceDigest, err := corekms.BindingSHA256(binding.InstanceID)
	if err != nil {
		return nil, err
	}
	slotDigest, err := corekms.BindingSHA256(binding.SlotID)
	if err != nil {
		return nil, err
	}
	return corekms.BindingContext{
		contextKeyVersion:        EncryptionContextVersion,
		contextKeyInstanceSHA256: instanceDigest,
		contextKeySlotSHA256:     slotDigest,
		contextKeyPayloadVersion: "1",
		contextKeyPurpose:        purpose,
	}, nil
}

func validateBindingContext(binding corekms.BindingContext) error {
	if len(binding) != 5 || binding[contextKeyVersion] != EncryptionContextVersion ||
		binding[contextKeyPayloadVersion] != "1" ||
		(binding[contextKeyPurpose] != EncryptionContextPurposePrimary && binding[contextKeyPurpose] != EncryptionContextPurposeRecovery) ||
		!lowercaseSHA256.MatchString(binding[contextKeyInstanceSHA256]) ||
		!lowercaseSHA256.MatchString(binding[contextKeySlotSHA256]) {
		return errors.New("AWS KMS Encryption Context is incomplete or invalid")
	}
	return nil
}
