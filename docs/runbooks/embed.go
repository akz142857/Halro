package runbooks

import _ "embed"

// The recovery documents are embedded into the release so an operator does
// not depend on GitHub, network access, or a moving branch during an incident.

//go:embed m11-kms-key-lifecycle.md
var KMSKeyLifecycle []byte

//go:embed m11-kms-disaster-recovery.md
var KMSDisasterRecovery []byte

// A leaked Gateway Key is the most likely credential incident: it is the only
// credential Halro hands to an application developer. The procedure is embedded
// for the same reason as the others — during an incident the operator should
// not be reading a moving branch.

//go:embed gateway-key-compromise.md
var GatewayKeyCompromise []byte

// Both of these describe conditions the operator meets while the instance is
// the thing that is broken: the data plane refusing every request, or a Master
// Key rotation that stopped half way. Reaching for them over the network is
// exactly what may not be available, and unlike the KMS pair they apply in the
// default file mode, so they ship in every binary.

//go:embed configuration-stale.md
var ConfigurationStale []byte

//go:embed file-master-key-rotation.md
var FileMasterKeyRotation []byte
