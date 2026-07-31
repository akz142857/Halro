package webui

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestHandlerServesSPAWithoutCachingEntry(t *testing.T) {
	for _, route := range []string{"/admin", "/admin/", "/admin/projects", "/admin/providers/id"} {
		request := httptest.NewRequest(http.MethodGet, route, nil)
		response := httptest.NewRecorder()
		Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK ||
			response.Header().Get("Cache-Control") != "no-store" ||
			!strings.Contains(response.Body.String(), `<div id="root"></div>`) {
			t.Fatalf("%s status=%d cache=%q body=%s", route, response.Code,
				response.Header().Get("Cache-Control"), response.Body.String())
		}
	}
}

func TestHandlerServesHashedAssetsAsImmutable(t *testing.T) {
	indexRequest := httptest.NewRequest(http.MethodGet, "/admin", nil)
	indexResponse := httptest.NewRecorder()
	Handler().ServeHTTP(indexResponse, indexRequest)
	match := regexp.MustCompile(`/admin/(assets/[^"]+\.js)`).FindStringSubmatch(indexResponse.Body.String())
	if len(match) != 2 {
		t.Fatalf("index did not contain a hashed JavaScript asset: %s", indexResponse.Body.String())
	}
	request := httptest.NewRequest(http.MethodGet, "/admin/"+match[1], nil)
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		response.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" ||
		response.Header().Get("ETag") == "" {
		t.Fatalf("asset status=%d cache=%q etag=%q", response.Code,
			response.Header().Get("Cache-Control"), response.Header().Get("ETag"))
	}
}

func TestHandlerNeverFallsBackForAdminAPIOrMissingAsset(t *testing.T) {
	for _, route := range []string{
		"/admin/api/v1/unknown",
		"/admin/assets/missing.js",
		"/admin/../secrets.enc",
	} {
		request := httptest.NewRequest(http.MethodGet, route, nil)
		response := httptest.NewRecorder()
		Handler().ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s status=%d", route, response.Code)
		}
	}
}
