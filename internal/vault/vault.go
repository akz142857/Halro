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

func (v *Vault) EncryptAdminMFA(authenticatorID, username string, plaintext []byte) ([]byte, error) {
	return v.encryptScoped("admin-mfa", authenticatorID, username, plaintext)
}

func (v *Vault) DecryptAdminMFA(authenticatorID, username string, envelope []byte) ([]byte, error) {
	return v.decryptScoped("admin-mfa", authenticatorID, username, envelope)
}

// EncryptFailurePayload seals the request and upstream answer captured for one
// failed request.
//
// This is the only material Halro stores that a caller wrote — a prompt, tool
// arguments, an upstream error body — so it is bound to both the request it
// belongs to and the project that paid for it. A record lifted out of one
// install's directory and dropped into another's, or renamed onto a different
// request ID, fails to open rather than opening as somebody else's traffic.
func (v *Vault) EncryptFailurePayload(requestID, projectID string, plaintext []byte) ([]byte, error) {
	return v.encryptScoped("failure-payload", requestID, projectID, plaintext)
}

func (v *Vault) DecryptFailurePayload(requestID, projectID string, envelope []byte) ([]byte, error) {
	return v.decryptScoped("failure-payload", requestID, projectID, envelope)
}

// EncryptResourceObject seals one provider object — an uploaded batch input, a
// batch result, a deferred response — under the resource that owns it and the
// project that paid for it.
//
// These bytes are the same class of material EncryptFailurePayload guards: a
// prompt the caller wrote, or output a model produced. They used to be written
// to the object directory in the clear, which held them to a lower standard
// than the identical bytes captured from a failed request. Binding the seal to
// both identifiers means an object renamed onto another record, or lifted into
// another install's directory, fails to open rather than opening as somebody
// else's traffic.
func (v *Vault) EncryptResourceObject(resourceID, projectID string, plaintext []byte) ([]byte, error) {
	return v.encryptScoped("resource-object", resourceID, projectID, plaintext)
}

func (v *Vault) DecryptResourceObject(resourceID, projectID string, envelope []byte) ([]byte, error) {
	return v.decryptScoped("resource-object", resourceID, projectID, envelope)
}

// SealedEnvelope reports whether the bytes carry the envelope header every
// scoped seal writes. It answers "was this written by a build that sealed" and
// nothing else: a true answer does not mean the envelope opens, and separating
// the two lets a caller reclaim a plaintext leftover quietly while treating an
// envelope that refuses to open as the far louder event it is.
func SealedEnvelope(data []byte) bool {
	return len(data) >= 6 && string(data[:4]) == envelopeMagic && data[4] == envelopeVersion
}

func (v *Vault) encryptScoped(kind, id, audience string, plaintext []byte) ([]byte, error) {
	aead, aad, err := v.scopedAEAD(kind, id, audience)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	sealed := aead.Seal(nil, nonce, plaintext, aad)
	envelope := make([]byte, 6+len(nonce)+len(sealed))
	copy(envelope[:4], envelopeMagic)
	envelope[4] = envelopeVersion
	envelope[5] = byte(len(nonce))
	copy(envelope[6:], nonce)
	copy(envelope[6+len(nonce):], sealed)
	return envelope, nil
}

func (v *Vault) decryptScoped(kind, id, audience string, envelope []byte) ([]byte, error) {
	if len(envelope) < 6 || string(envelope[:4]) != envelopeMagic || envelope[4] != envelopeVersion {
		return nil, errors.New("invalid secret envelope")
	}
	aead, aad, err := v.scopedAEAD(kind, id, audience)
	if err != nil {
		return nil, err
	}
	n := int(envelope[5])
	if n != aead.NonceSize() || len(envelope) < 6+n {
		return nil, errors.New("invalid secret nonce")
	}
	plaintext, err := aead.Open(nil, envelope[6:6+n], envelope[6+n:], aad)
	if err != nil {
		return nil, errors.New("secret authentication failed")
	}
	return plaintext, nil
}

func (v *Vault) scopedAEAD(kind, id, audience string) (cipher.AEAD, []byte, error) {
	if len(v.masterKey) != MasterKeySize || kind == "" || id == "" || audience == "" {
		return nil, nil, errors.New("invalid secret scope")
	}
	aad, err := credentialAAD(id, kind, audience)
	if err != nil {
		return nil, nil, err
	}
	key, err := hkdf.Key(sha256.New, v.masterKey, []byte("halro:vault:v1"), "halro:"+kind+"-key:v1:"+id+":"+audience, 32)
	if err != nil {
		return nil, nil, err
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	aead, err := cipher.NewGCM(block)
	return aead, aad, err
}

func (v *Vault) credentialAEAD(credentialID, providerType, audience string) (cipher.AEAD, error) {
	if len(v.masterKey) != MasterKeySize {
		return nil, errors.New("vault is closed")
	}
	info := "halro:credential-key:v1:" + credentialID + ":" + providerType + ":" + audience
	key, err := hkdf.Key(sha256.New, v.masterKey, []byte("halro:vault:v1"), info, 32)
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
	values := []string{"halro:credential:v1", credentialID, providerType, audience}
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
