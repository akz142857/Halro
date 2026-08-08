package awskms

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	corekms "github.com/akz142857/Halro/internal/kms"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

func TestRealAWSKMSWorkloadIdentityEncryptDecrypt(t *testing.T) {
	if os.Getenv("HALRO_AWS_KMS_REAL") != "1" {
		t.Skip("set HALRO_AWS_KMS_REAL=1 for real AWS KMS evidence")
	}
	keyARN := os.Getenv("HALRO_AWS_KMS_KEY_ARN")
	partition, region, account, _, ok := parseKeyARN(keyARN)
	if !ok || partition == "" {
		t.Fatal("HALRO_AWS_KMS_KEY_ARN must be a full customer managed Key ARN")
	}
	options := Options{Region: region, Account: account, KeyARN: keyARN, Algorithm: SymmetricDefaultAlgorithm}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	identitySource, err := realWorkloadIdentitySource(ctx, region)
	if err != nil {
		t.Fatal(err)
	}
	wrapper, err := New(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := corekms.NewExecutor(wrapper, corekms.RetryPolicy{
		CallTimeout: 5 * time.Second, StartupDeadline: time.Minute,
		InitialBackoff: 250 * time.Millisecond, MaxBackoff: 5 * time.Second, MaxAttempts: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := corekms.PayloadBinding{InstanceID: "m11-real-smoke", SlotID: "slot_aws_smoke"}
	contextBinding, err := NewBindingContext(binding, EncryptionContextPurposePrimary)
	if err != nil {
		t.Fatal(err)
	}
	masterKey := make([]byte, corekms.MasterKeyBytes)
	if _, err := rand.Read(masterKey); err != nil {
		t.Fatal(err)
	}
	defer clear(masterKey)
	payload, err := corekms.EncodeProtectedPayload(binding, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(payload)
	wrapped, err := executor.Wrap(ctx, corekms.WrapRequest{
		KeyReference: keyARN, Algorithm: SymmetricDefaultAlgorithm, Plaintext: payload, BindingContext: contextBinding,
	})
	if err != nil {
		t.Fatal(err)
	}
	unwrapped, err := executor.Unwrap(ctx, corekms.UnwrapRequest{
		KeyReference: keyARN, Algorithm: SymmetricDefaultAlgorithm, Ciphertext: wrapped.Ciphertext, BindingContext: contextBinding,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer clear(unwrapped.Plaintext)
	decoded, err := corekms.DecodeProtectedPayload(binding, unwrapped.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(decoded)
	if !bytes.Equal(decoded, masterKey) {
		t.Fatal("real AWS KMS changed the Master Key")
	}
	evidence := map[string]any{
		"schema_version": 1, "recorded_at": time.Now().UTC(), "region": region,
		"account_sha256": digestAWSString(account), "key_arn_sha256": digestAWSString(keyARN),
		"encrypt_request_id_sha256": digestAWSString(wrapped.ProviderRequestID),
		"decrypt_request_id_sha256": digestAWSString(unwrapped.ProviderRequestID),
		"ciphertext_bytes":          len(wrapped.Ciphertext), "protected_payload_bytes": len(payload),
		"encryption_context_version": EncryptionContextVersion,
		"workload_identity":          identitySource, "result": "success",
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("M11_AWS_KMS_EVIDENCE=%s\n", encoded)
	if privatePath := os.Getenv("HALRO_AWS_KMS_PRIVATE_EVIDENCE_FILE"); privatePath != "" {
		privateEvidence, marshalErr := json.Marshal(map[string]string{
			"account": account, "key_arn": keyARN,
			"encrypt_request_id": wrapped.ProviderRequestID,
			"decrypt_request_id": unwrapped.ProviderRequestID,
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if writeErr := os.WriteFile(privatePath, privateEvidence, 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
}

func realWorkloadIdentitySource(ctx context.Context, region string) (string, error) {
	config, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return "", err
	}
	credentials, err := config.Credentials.Retrieve(ctx)
	if err != nil {
		return "", err
	}
	if !workloadIdentitySource(credentials.Source) {
		return "", fmt.Errorf("AWS credential source %q is not an approved Workload Identity provider", credentials.Source)
	}
	return credentials.Source, nil
}

func digestAWSString(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
