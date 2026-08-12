package app

import (
	"net/http"

	"github.com/akz142857/Halro/docs/runbooks"
	"github.com/akz142857/Halro/internal/config"
)

func (r *Runtime) adminMasterKeyLifecycleRunbook(writer http.ResponseWriter, request *http.Request) {
	if r.config.Storage.MasterKey.Mode != config.MasterKeyModeKeySlots {
		http.NotFound(writer, request)
		return
	}
	writeMasterKeyRunbook(writer, runbooks.KMSKeyLifecycle)
}

func (r *Runtime) adminMasterKeyRecoveryRunbook(writer http.ResponseWriter, request *http.Request) {
	if r.config.Storage.MasterKey.Mode != config.MasterKeyModeKeySlots {
		http.NotFound(writer, request)
		return
	}
	writeMasterKeyRunbook(writer, runbooks.KMSDisasterRecovery)
}

// adminGatewayKeyCompromiseRunbook is not gated on the Master Key custody mode,
// unlike the two above: a leaked Gateway Key is possible under every
// configuration, and a runbook that is missing exactly when it is needed is
// worse than no runbook at all.
func (r *Runtime) adminGatewayKeyCompromiseRunbook(writer http.ResponseWriter, request *http.Request) {
	writeMasterKeyRunbook(writer, runbooks.GatewayKeyCompromise)
}

// Neither of these is gated on custody mode either: configuration_stale can
// happen in any deployment, and the file-mode rotation procedure is the one
// the default configuration needs.
func (r *Runtime) adminConfigurationStaleRunbook(writer http.ResponseWriter, request *http.Request) {
	writeMasterKeyRunbook(writer, runbooks.ConfigurationStale)
}

func (r *Runtime) adminFileMasterKeyRotationRunbook(writer http.ResponseWriter, request *http.Request) {
	if r.config.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots {
		http.NotFound(writer, request)
		return
	}
	writeMasterKeyRunbook(writer, runbooks.FileMasterKeyRotation)
}

func writeMasterKeyRunbook(writer http.ResponseWriter, content []byte) {
	writer.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	writer.Header().Set("Cache-Control", "private, no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(content)
}
