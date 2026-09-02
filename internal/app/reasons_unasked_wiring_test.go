package app

import (
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/modelcatalog"
)

// The middle link of the chain: catalogue -> target -> router filter.
//
// The two ends were covered and this was not, which was found the way these
// things are found — by backing the change out and watching nothing fail. A
// registry that stops reading the catalogue leaves every filter downstream
// correct and inert, because ReasonsUnasked is simply never set, and the failure
// it exists to prevent comes back silently.
func TestTheRegistryReadsReasonsUnaskedFromTheCatalogue(t *testing.T) {
	catalog := modelcatalog.Builtin()
	instance := domain.ProviderInstance{Type: domain.ProviderKimi}
	deployment := func(profile domain.ProviderProfileID, model string) domain.Deployment {
		return domain.Deployment{ProfileID: profile, ProviderModel: model}
	}
	for _, test := range []struct {
		name       string
		instance   domain.ProviderInstance
		deployment domain.Deployment
		want       bool
	}{
		{
			// The line with no off switch: `invalid thinking: only type=enabled is
			// allowed for this model`.
			name: "a model that always reasons", instance: instance,
			deployment: deployment(domain.ProfileKimiChat, "kimi-k2.7-code"), want: true,
		},
		{
			name: "the same line's highspeed twin", instance: instance,
			deployment: deployment(domain.ProfileKimiChat, "kimi-k2.7-code-highspeed"), want: true,
		},
		{
			// Measured taking an off switch, against its own documentation and its
			// own /v1/models metadata.
			name: "a model that can be switched off", instance: instance,
			deployment: deployment(domain.ProfileKimiChat, "kimi-k3"), want: false,
		},
		{
			// Not covered by the catalogue, which is what an operator-declared
			// deployment is. False is the only workable answer: routing away
			// everything unknown would refuse every one of them.
			name: "a model this build does not know", instance: instance,
			deployment: deployment(domain.ProfileKimiChat, "kimi-k9-unreleased"), want: false,
		},
		{
			// MiniMax's M2 line reasons unasked on Responses and its entries were
			// removed from that profile for it. On Chat, where it is offered, the
			// answer decodes — so the mark belongs to the pair, not the model.
			name:       "the same model on a profile whose decoder carries it",
			instance:   domain.ProviderInstance{Type: domain.ProviderMiniMax},
			deployment: deployment(domain.ProfileMiniMaxChat, "MiniMax-M2.1"), want: false,
		},
	} {
		if got := reasonsUnasked(catalog, test.instance, test.deployment); got != test.want {
			t.Errorf("%s: reasonsUnasked = %v, want %v", test.name, got, test.want)
		}
	}
	// A registry built before the catalogue is available must not claim the
	// property either way; false keeps it inert rather than refusing everything.
	if reasonsUnasked(nil, instance, deployment(domain.ProfileKimiChat, "kimi-k2.7-code")) {
		t.Fatal("a nil catalogue answered true, which would refuse routes on no evidence")
	}
}
