package app

import "net/http"

// recoverPanics is deliberately value-blind: panic values, request paths,
// headers, and bodies may contain credentials or prompts and are never logged.
func (r *Runtime) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		tracked := &panicResponseWriter{ResponseWriter: writer}
		defer func() {
			if recover() == nil {
				return
			}
			r.logger.Error("http handler panic recovered", "component", "http")
			if !tracked.wroteHeader {
				writeJSON(tracked, http.StatusInternalServerError, map[string]any{
					"error": "internal server error",
				})
			}
		}()
		next.ServeHTTP(tracked, request)
	})
}

type panicResponseWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *panicResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *panicResponseWriter) WriteHeader(status int) {
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *panicResponseWriter) Write(payload []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(payload)
}

func (w *panicResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
