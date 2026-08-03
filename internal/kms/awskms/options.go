package awskms

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

const (
	Provider                         = "aws-kms"
	SymmetricDefaultAlgorithm        = "SYMMETRIC_DEFAULT"
	MaxAWSCiphertextBytes            = 6144
	EncryptionContextVersion         = "1"
	EncryptionContextPurposePrimary  = "primary"
	EncryptionContextPurposeRecovery = "recovery"
)

var (
	awsAccountID = regexp.MustCompile(`^[0-9]{12}$`)
	awsRegion    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	awsPartition = regexp.MustCompile(`^aws(?:-[a-z0-9-]+)?$`)
	awsKeyID     = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)
)

type Options struct {
	Region    string
	Account   string
	KeyARN    string
	Endpoint  string
	Algorithm string
}

func (o Options) Normalized() Options {
	if o.Algorithm == "" {
		o.Algorithm = SymmetricDefaultAlgorithm
	}
	if strings.HasSuffix(o.Endpoint, "/") {
		o.Endpoint = strings.TrimSuffix(o.Endpoint, "/")
	}
	return o
}

func (o Options) Validate() error {
	o = o.Normalized()
	if !awsRegion.MatchString(o.Region) {
		return errors.New("AWS KMS region allowlist is invalid")
	}
	if !awsAccountID.MatchString(o.Account) {
		return errors.New("AWS KMS account allowlist is invalid")
	}
	partition, region, account, resource, ok := parseKeyARN(o.KeyARN)
	if !ok || !awsPartition.MatchString(partition) || region != o.Region || account != o.Account ||
		!strings.HasPrefix(resource, "key/") || !awsKeyID.MatchString(strings.TrimPrefix(resource, "key/")) {
		return errors.New("AWS KMS Key ARN is outside the trusted region/account/key allowlist")
	}
	if o.Algorithm != SymmetricDefaultAlgorithm {
		return errors.New("AWS KMS algorithm allowlist permits only SYMMETRIC_DEFAULT")
	}
	if o.Endpoint != "" {
		endpoint, err := url.Parse(o.Endpoint)
		if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil ||
			(endpoint.Path != "" && endpoint.Path != "/") || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return errors.New("AWS KMS endpoint allowlist requires an HTTPS origin")
		}
	}
	return nil
}

func parseKeyARN(value string) (partition, region, account, resource string, ok bool) {
	parts := strings.SplitN(value, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "kms" {
		return "", "", "", "", false
	}
	return parts[1], parts[3], parts[4], parts[5], true
}
