package app

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// The Admin mutation path's shared vocabulary: how a revision precondition is
// read, and the handful of refusals every handler needs to be able to state.
//
// These lived in admin_projects.go, which made the file that defines project
// CRUD also the file the other twenty admin handlers depended on. Nothing here
// is about projects, so nothing here belongs there — and a future split of this
// package has to move this vocabulary first either way.

func requireRevision(writer http.ResponseWriter, request *http.Request) (uint64, bool) {
	raw := request.Header.Get("If-Match")
	if len(raw) < 3 || raw[0] != '"' || raw[len(raw)-1] != '"' || strings.Contains(raw[1:len(raw)-1], `"`) {
		writeJSON(writer, http.StatusPreconditionRequired, map[string]string{"error": "If-Match revision is required"})
		return 0, false
	}
	revision, err := strconv.ParseUint(raw[1:len(raw)-1], 10, 64)
	if err != nil || revision == 0 {
		writeJSON(writer, http.StatusPreconditionRequired, map[string]string{"error": "If-Match revision is required"})
		return 0, false
	}
	return revision, true
}

func adminMutationError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, boltstore.ErrNotFound):
		adminNotFound(writer)
	case errors.Is(err, boltstore.ErrRevisionConflict):
		adminPreconditionFailed(writer)
	case errors.Is(err, boltstore.ErrAlreadyExists), errors.Is(err, boltstore.ErrKeyHashConflict):
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "resource conflict"})
	default:
		adminBadRequest(writer, err.Error())
	}
}

func adminBadRequest(writer http.ResponseWriter, message string) {
	writeJSON(writer, http.StatusBadRequest, map[string]string{"error": message})
}

func adminBadRequestCode(writer http.ResponseWriter, code, message string) {
	writeJSON(writer, http.StatusBadRequest, map[string]string{"code": code, "error": message})
}

func adminPreconditionFailed(writer http.ResponseWriter) {
	writeJSON(writer, http.StatusPreconditionFailed, map[string]string{"error": "resource revision conflict"})
}

func adminAuditError(writer http.ResponseWriter) {
	writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "audit unavailable"})
}

func adminNotFound(writer http.ResponseWriter) {
	writeJSON(writer, http.StatusNotFound, map[string]string{"error": "resource not found"})
}

func adminStoreError(writer http.ResponseWriter) {
	writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "metadata unavailable"})
}
