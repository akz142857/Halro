package provider

import (
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

// A subscription product that routes a request onto the tier its plan entitles
// answers as a different model than the one asked for. Measured on BigModel's
// GLM Coding Plan: eight of the ten identifiers its own /models route lists are
// answered by one of two models.
//
// The danger is not the routing, which is the product working as sold. It is
// that a probe measuring glm-5.3-flash would otherwise leave *verified* evidence
// against a glm-4.6 deployment — capabilities recorded for a model that never
// ran, on the one record the router later trusts.
func TestAProbeAnsweredByAnotherModelLeavesNoVerifiedEvidence(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		requested string
		answered  string
		verified  bool
		class     string
	}{
		{name: "the model that was asked for", requested: "glm-5.3", answered: "glm-5.3", verified: true},
		{name: "the upstream normalises the case it echoes", requested: "GLM-5.3", answered: "glm-5.3", verified: true},
		{name: "the plan routed it elsewhere", requested: "glm-4.6", answered: "glm-5.3-flash", class: "model_substituted"},
		{name: "the upstream said nothing", requested: "glm-4.6", answered: "", verified: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := domain.CapabilityProbeResult{Status: domain.ProbeSupported, Evidence: domain.EvidenceVerified}
			applySubstitutionGuard(&result, testCase.requested, testCase.answered)
			if testCase.verified {
				if result.Evidence != domain.EvidenceVerified || result.ErrorClass != "" {
					t.Fatalf("evidence was discarded for a model that answered as itself: %+v", result)
				}
				return
			}
			if result.Evidence == domain.EvidenceVerified {
				t.Fatalf("another model's answer was recorded as verified evidence: %+v", result)
			}
			if result.ErrorClass != testCase.class {
				t.Fatalf("error class is %q, want %q — an operator has to be able to tell this from a"+
					" refusal", result.ErrorClass, testCase.class)
			}
			if result.Status != domain.ProbeAssertionFailed {
				t.Fatalf("status is %q, want an assertion failure", result.Status)
			}
		})
	}
}

// And it must not fire where a different name is the ordinary answer. An Azure
// deployment is addressed by its deployment name and answers with the underlying
// model; an OpenAI alias resolves to a dated snapshot. Treating either as a
// substitution would throw away every capability those platforms measure.
func TestTheSubstitutionGuardIsScopedToUpstreamsThatEchoTheIdentifier(t *testing.T) {
	for _, profile := range []domain.ProviderProfileID{
		domain.ProfileAzureChatEmbeddings, domain.ProfileOpenAIChatEmbeddings,
		domain.ProfileOpenAIResponses, domain.ProfileBedrockMantleChat,
		domain.ProfileDeepSeekChat, domain.ProfileKimiChat, domain.ProfileMiniMaxChat,
	} {
		if profileEchoesTheModelItWasGiven(profile) {
			t.Fatalf("%s is treated as echoing its model identifier; a deployment name or a dated"+
				" snapshot would then be read as a substitution", profile)
		}
	}
	for _, profile := range []domain.ProviderProfileID{
		domain.ProfileBigModelCNChatEmbeddings, domain.ProfileBigModelGlobalChat,
		domain.ProfileBigModelCNCodingChat,
	} {
		if !profileEchoesTheModelItWasGiven(profile) {
			t.Fatalf("%s was measured echoing the identifier it was given and is not covered", profile)
		}
	}
}
