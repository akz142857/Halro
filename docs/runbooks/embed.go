package runbooks

import _ "embed"

// The recovery documents are embedded into the release so an operator does
// not depend on GitHub, network access, or a moving branch during an incident.

//go:embed m11-kms-key-lifecycle.md
var KMSKeyLifecycle []byte

//go:embed m11-kms-disaster-recovery.md
var KMSDisasterRecovery []byte
