package app

import (
	"testing"

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/provider"
)

func TestEnabledProviderBindingRequiresIdentityWhenAmbiguous(t *testing.T) {
	instance := domain.ProviderInstance{ID: "provider", Bindings: []domain.ProviderProfileBinding{
		{ID: "chat", Enabled: true},
		{ID: "media", Enabled: true},
		{ID: "disabled", Enabled: false},
	}}
	if _, err := enabledProviderBinding(instance, ""); err == nil {
		t.Fatal("ambiguous provider binding selection did not fail closed")
	}
	selected, err := enabledProviderBinding(instance, "media")
	if err != nil || selected.ID != "media" {
		t.Fatalf("selected=%#v err=%v", selected, err)
	}
	if _, err := enabledProviderBinding(instance, "disabled"); err == nil {
		t.Fatal("disabled provider binding was selected")
	}
}

func TestAdapterForDeploymentUsesBindingIdentity(t *testing.T) {
	registry := provider.NewRegistry()
	chat := &adminProbeAdapter{}
	media := &adminProbeAdapter{}
	if err := registry.RegisterBindingAdapter("provider", "chat", chat); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterBindingAdapter("provider", "media", media); err != nil {
		t.Fatal(err)
	}
	instance := domain.ProviderInstance{ID: "provider"}
	adapter, ok := adapterForDeployment(registry, instance, domain.Deployment{ProviderID: "provider", BindingID: "media"})
	if !ok || adapter != media {
		t.Fatalf("adapter=%#v ok=%v", adapter, ok)
	}
}

func TestMatchingBindingIDFailsClosedWhenProfileIsAmbiguous(t *testing.T) {
	instance := domain.ProviderInstance{Bindings: []domain.ProviderProfileBinding{
		{ID: "first", ProfileID: domain.ProfileOpenAIChatEmbeddings, Enabled: true},
		{ID: "second", ProfileID: domain.ProfileOpenAIChatEmbeddings, Enabled: true},
	}}
	if got := matchingBindingID(instance, domain.ProfileOpenAIChatEmbeddings); got != "" {
		t.Fatalf("ambiguous profile resolved to %q", got)
	}
}
