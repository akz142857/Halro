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

func writeMasterKeyRunbook(writer http.ResponseWriter, content []byte) {
	writer.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	writer.Header().Set("Cache-Control", "private, no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(content)
}
