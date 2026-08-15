package app

import "fmt"

// awsCredentialForTest is the shape an AWS SigV4 credential actually has, for
// the fixtures that need one to be accepted.
//
// It takes the region because the region is not decoration: the signer pins the
// host to `bedrock-runtime.<region>.amazonaws.com`, so a document naming another
// region is a credential that can never sign for the endpoint it is bound to.
// The write path refuses that now, which is why a fixture cannot go on passing
// a bare string here.
func awsCredentialForTest(region string) string {
	return fmt.Sprintf(
		`{"access_key_id":"AKIAIOSFODNN7EXAMPLE","secret_access_key":"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY","region":%q}`,
		region,
	)
}
