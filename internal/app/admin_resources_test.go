package app

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestWriteResourcePageSerializesEmptyItemsAsArray(t *testing.T) {
	request := httptest.NewRequest("GET", "/admin/api/v1/resources", nil)
	response := httptest.NewRecorder()

	writeResourcePage(response, request, []string(nil), func(item string) string { return item })

	var page struct {
		Items []string `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Items == nil {
		t.Fatalf("empty resource page must serialize items as [], body=%s", response.Body.String())
	}
}
