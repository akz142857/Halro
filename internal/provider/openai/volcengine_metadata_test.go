package openai

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

func TestVolcengineMetadataClaimsComeOnlyFromExplicitFields(t *testing.T) {
	payload, err := os.ReadFile("testdata/volcengine-models.json")
	if err != nil {
		t.Fatal(err)
	}
	adapter := newVolcengineTestAdapter(t, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(payload)))}, nil
	})
	items, err := adapter.ListInvocationTargets(context.Background(), domain.TargetQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		scope := domain.InvocationTargetScopeKey{ProviderID: "provider", TargetID: item.TargetID, TargetKind: domain.TargetModelID, BindingID: "binding", ProfileID: domain.ProfileVolcengineChat}
		claims := adapter.MapCapabilityClaims(item, scope, time.Now().UTC())
		if item.Availability == domain.AvailabilityUnavailable && len(claims) != 0 {
			t.Fatal("shutdown model claimed usable capabilities")
		}
		byName := map[string]domain.CapabilityClaim{}
		for _, claim := range claims {
			byName[claim.CapabilityID] = claim
			if claim.Scope != scope || claim.Source != domain.ClaimSourceProviderMetadata || claim.Validate() != nil {
				t.Fatalf("invalid scoped metadata claim: %+v", claim)
			}
		}
		if _, present := byName["streaming"]; present {
			t.Fatal("missing streaming flag was fabricated")
		}
		if _, present := byName["fetched_image"]; present {
			t.Fatal("image input was treated as proof of remote URL fetch")
		}
		if item.TargetID == "doubao-seed-2-0-mini-260428" {
			for _, name := range []string{"chat", "vision", "reasoning", "tools", "json_object", "structured_outputs"} {
				if byName[name].Status != domain.ClaimSupported {
					t.Fatalf("explicit %s support not mapped", name)
				}
			}
			if item.Metadata.MaxContextTokens != 262144 || item.Metadata.MaxOutputTokens != 131072 {
				t.Fatal("provider token limits were lost")
			}
		}
		if item.TargetID == "doubao-seedance-2-5-260628" && len(claims) != 0 {
			t.Fatal("video model acquired Chat capabilities")
		}
	}
	unknown := domain.InvocationTargetDescriptor{TargetID: "invented", MetadataSource: domain.MetadataSourceNone}
	if len(adapter.MapCapabilityClaims(unknown, domain.InvocationTargetScopeKey{}, time.Now())) != 0 {
		t.Fatal("typed identifier acquired provider metadata")
	}
}

func TestVolcengineExplicitFalseIsNegativeButMissingIsUnknown(t *testing.T) {
	payload := `{"data":[{"id":"known","task_type":["TextGeneration"],"modalities":{"output_modalities":["text"]},"features":{"tools":{"function_calling":false},"structured_outputs":{"json_object":false}}}]}`
	adapter := newVolcengineTestAdapter(t, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(payload))}, nil
	})
	items, err := adapter.ListInvocationTargets(context.Background(), domain.TargetQuery{})
	if err != nil {
		t.Fatal(err)
	}
	claims := adapter.MapCapabilityClaims(items[0], domain.InvocationTargetScopeKey{}, time.Now())
	statuses := map[string]domain.ClaimStatus{}
	for _, claim := range claims {
		statuses[claim.CapabilityID] = claim.Status
	}
	if statuses["chat"] != domain.ClaimSupported || statuses["tools"] != domain.ClaimUnsupported || statuses["json_object"] != domain.ClaimUnsupported {
		t.Fatalf("explicit fields lost: %v", statuses)
	}
	if _, ok := statuses["structured_outputs"]; ok {
		t.Fatal("missing schema flag became a negative claim")
	}
}
