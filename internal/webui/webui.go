package webui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed dist
var embedded embed.FS

type handler struct {
	content fs.FS
}

func Handler() http.Handler {
	content, err := fs.Sub(embedded, "dist")
	if err != nil {
		panic(err)
	}
	return &handler{content: content}
}

func (h *handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD")
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	relative := strings.TrimPrefix(request.URL.Path, "/admin")
	relative = strings.TrimPrefix(relative, "/")
	if relative == "api" || strings.HasPrefix(relative, "api/") {
		http.NotFound(writer, request)
		return
	}
	if strings.Contains(relative, `\`) || strings.Contains(relative, "..") {
		http.NotFound(writer, request)
		return
	}
	relative = path.Clean(relative)
	if relative == "." {
		relative = "index.html"
	}
	if h.serveFile(writer, request, relative) {
		return
	}
	if path.Ext(relative) != "" {
		http.NotFound(writer, request)
		return
	}
	h.serveFile(writer, request, "index.html")
}

func (h *handler) serveFile(
	writer http.ResponseWriter,
	request *http.Request,
	name string,
) bool {
	raw, err := fs.ReadFile(h.content, name)
	if err != nil {
		return false
	}
	if name == "index.html" {
		writer.Header().Set("Cache-Control", "no-store")
	} else {
		writer.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	contentType := mime.TypeByExtension(path.Ext(name))
	if contentType != "" {
		writer.Header().Set("Content-Type", contentType)
	}
	digest := sha256.Sum256(raw)
	writer.Header().Set("ETag", `"`+hex.EncodeToString(digest[:16])+`"`)
	http.ServeContent(writer, request, name, time.Time{}, bytes.NewReader(raw))
	return true
}
