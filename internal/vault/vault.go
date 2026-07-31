package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	envelopeVersion = 1
	envelopeMagic   = "HVLT"
)

type Vault struct {
	masterKey []byte
}

func New(masterKey []byte) (*Vault, error) {
	if len(masterKey) != MasterKeySize {
		return nil, fmt.Errorf("master key must be %d bytes", MasterKeySize)
	}
	keyCopy := make([]byte, len(masterKey))
	copy(keyCopy, masterKey)
	return &Vault{masterKey: keyCopy}, nil
}

func (v *Vault) Close() {
	if v == nil {
		return
	}
	clear(v.masterKey)
	v.masterKey = nil
}

func (v *Vault) EncryptCredential(credentialID, providerType, audience string, plaintext []byte) ([]byte, error) {
	aad, err := credentialAAD(credentialID, providerType, audience)
	if err != nil {
		return nil, err
	}
	aead, err := v.credentialAEAD(credentialID, providerType, audience)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate credential nonce: %w", err)
	}
	sealed := aead.Seal(nil, nonce, plaintext, aad)
	envelope := make([]byte, 4+1+1+len(nonce)+len(sealed))
	copy(envelope[:4], envelopeMagic)
	envelope[4] = envelopeVersion
	envelope[5] = byte(len(nonce))
	copy(envelope[6:], nonce)
	copy(envelope[6+len(nonce):], sealed)
	return envelope, nil
}

func (v *Vault) DecryptCredential(credentialID, providerType, audience string, envelope []byte) ([]byte, error) {
	if len(envelope) < 6 || string(envelope[:4]) != envelopeMagic {
		return nil, errors.New("invalid credential envelope")
	}
	if envelope[4] != envelopeVersion {
		return nil, fmt.Errorf("unsupported credential envelope version %d", envelope[4])
	}
	nonceLength := int(envelope[5])
	if nonceLength <= 0 || len(envelope) < 6+nonceLength {
		return nil, errors.New("invalid credential nonce")
	}
	aad, err := credentialAAD(credentialID, providerType, audience)
	if err != nil {
		return nil, err
	}
	aead, err := v.credentialAEAD(credentialID, providerType, audience)
	if err != nil {
		return nil, err
	}
	if nonceLength != aead.NonceSize() {
		return nil, errors.New("credential nonce size mismatch")
	}
	plaintext, err := aead.Open(nil, envelope[6:6+nonceLength], envelope[6+nonceLength:], aad)
	if err != nil {
		return nil, errors.New("credential authentication failed")
	}
	return plaintext, nil
}

func (v *Vault) credentialAEAD(credentialID, providerType, audience string) (cipher.AEAD, error) {
	if len(v.masterKey) != MasterKeySize {
		return nil, errors.New("vault is closed")
	}
	info := "heimdall:credential-key:v1:" + credentialID + ":" + providerType + ":" + audience
	key, err := hkdf.Key(sha256.New, v.masterKey, []byte("heimdall:vault:v1"), info, 32)
	if err != nil {
		return nil, fmt.Errorf("derive credential key: %w", err)
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create credential cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

func credentialAAD(credentialID, providerType, audience string) ([]byte, error) {
	if credentialID == "" || providerType == "" || audience == "" {
		return nil, errors.New("credential id, provider type, and audience are required")
	}
	values := []string{"heimdall:credential:v1", credentialID, providerType, audience}
	size := 0
	for _, value := range values {
		size += 4 + len(value)
	}
	aad := make([]byte, size)
	offset := 0
	for _, value := range values {
		binary.BigEndian.PutUint32(aad[offset:offset+4], uint32(len(value)))
		offset += 4
		copy(aad[offset:], value)
		offset += len(value)
	}
	return aad, nil
}
