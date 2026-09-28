package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

var haWriteResponseOutcomes = [...]string{
	"http_2xx", "http_4xx", "not_primary", "http_503", "timeout", "http_5xx", "canceled", "write_error",
}

// trackedHAWriteRoute is an explicit inventory of client write routes. It
// excludes reads, token counting, and unknown paths so a future route must be
// reviewed before its responses acquire a key-write interpretation.
func trackedHAWriteRoute(request *http.Request) bool {
	if request.Method != http.MethodPost {
		return false
	}
	switch request.URL.Path {
	case "/v1/chat/completions", "/v1/responses", "/v1/messages", "/v1/embeddings",
		"/v1/moderations", "/v1/images/generations", "/v1/audio/speech",
		"/v1/audio/transcriptions", "/v1/rerank", "/v1/async/invocations",
		"/v1/files", "/v1/batches", "/halro/v1/work-units", "/halro/v1/runs":
		return true
	}
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/"), "/")
	return len(parts) == 5 && parts[0] == "halro" && parts[1] == "v1" && parts[3] != "" &&
		(parts[2] == "work-units" && (parts[4] == "close" || parts[4] == "outcomes") ||
			parts[2] == "runs" && parts[4] == "close")
}

type haWriteResponseWriter struct {
	http.ResponseWriter
	status   int
	writeErr bool
}

func (w *haWriteResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *haWriteResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(body)
	if err != nil || n != len(body) {
		w.writeErr = true
	}
	return n, err
}

// Unwrap preserves ResponseController's deadline and flush access.
func (w *haWriteResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type haWriteFlushingResponseWriter struct{ *haWriteResponseWriter }

func (w *haWriteFlushingResponseWriter) Flush() {
	if err := http.NewResponseController(w.ResponseWriter).Flush(); err != nil {
		w.writeErr = true
	}
}

func (r *Runtime) trackHAWriteResponses(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !trackedHAWriteRoute(request) {
			next.ServeHTTP(writer, request)
			return
		}
		tracked := &haWriteResponseWriter{ResponseWriter: writer}
		defer func() {
			outcome := 5 // HTTP 5xx or missing terminal response.
			switch {
			case tracked.writeErr:
				outcome = 7
			case errors.Is(request.Context().Err(), context.DeadlineExceeded):
				outcome = 4
			case request.Context().Err() != nil:
				outcome = 6
			case tracked.status == http.StatusRequestTimeout || tracked.status == http.StatusGatewayTimeout:
				outcome = 4
			case tracked.status == http.StatusServiceUnavailable && r.replication != nil && r.replication.role != "primary":
				outcome = 2
			case tracked.status == http.StatusServiceUnavailable:
				outcome = 3
			case tracked.status >= 200 && tracked.status < 300:
				outcome = 0
			case tracked.status >= 400 && tracked.status < 500:
				outcome = 1
			}
			r.haWriteResponses[outcome].Add(1)
		}()
		if _, ok := writer.(http.Flusher); ok {
			next.ServeHTTP(&haWriteFlushingResponseWriter{tracked}, request)
		} else {
			next.ServeHTTP(tracked, request)
		}
	})
}
