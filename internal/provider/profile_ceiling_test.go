package provider

import (
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

// A capability ceiling is a claim about compiled code: every operation a profile
// offers is bound to a primitive in its manifest, and ProfileManifest.Validate
// refuses a manifest where the two do not line up. Nothing tied the ceiling to
// that, so a row could offer an operation with no primitive behind it — routing
// would then hand the connection a request it can only answer with an error,
// which is the failure the ceiling exists to prevent.
//
// This walks the domain table rather than a list of its own. A private list
// cannot be told when a profile is added, which is the whole reason the table
// became enumerable.
func TestCeilingWithinProfileManifestOperations(t *testing.T) {
	// Capabilities that name an operation. The rest — tools, vision, json_mode,
	// developer_role, reasoning, stream_usage, provider_executed_tools — shape a
	// chat request rather than reaching a separate endpoint, so no manifest
	// operation corresponds to them.
	operationFor := map[string][]Operation{
		"chat":           {OperationChat, OperationMessages},
		"streaming":      {OperationChatStream, OperationMessagesStream},
		"embeddings":     {OperationEmbeddings},
		"moderations":    {OperationModerations},
		"images":         {OperationImages},
		"transcriptions": {OperationTranscriptions},
		"speech":         {OperationSpeech},
		"files":          {OperationFiles},
		"batches":        {OperationBatches},
		"rerank":         {OperationRerank},
		"async_generate": {OperationAsyncInvoke},
	}

	for _, profile := range domain.AllProviderProfiles() {
		manifest, ok := BuiltinProfile(profile.ID)
		if !ok {
			t.Errorf("%s is in the capability table but has no profile manifest", profile.ID)
			continue
		}
		declared := make(map[Operation]struct{}, len(manifest.Operations))
		for _, operation := range manifest.Operations {
			declared[operation] = struct{}{}
		}
		for name, operations := range operationFor {
			if !capabilityEnabled(profile.Ceiling, name) {
				continue
			}
			served := false
			for _, operation := range operations {
				if _, ok := declared[operation]; ok {
					served = true
					break
				}
			}
			if !served {
				t.Errorf("%s ceiling offers %q but its manifest binds no primitive for it (operations: %v)",
					profile.ID, name, manifest.Operations)
			}
		}
	}
}

// The reverse direction is not an error — a manifest may serve an operation the
// ceiling does not offer yet — but it is worth reporting, because it is usually
// a capability that was implemented and never exposed.
func TestManifestOperationsNotOfferedByCeiling(t *testing.T) {
	capabilityFor := map[Operation]string{
		OperationChat: "chat", OperationMessages: "chat",
		OperationChatStream: "streaming", OperationMessagesStream: "streaming",
		OperationEmbeddings: "embeddings", OperationModerations: "moderations",
		OperationImages: "images", OperationTranscriptions: "transcriptions",
		OperationSpeech: "speech", OperationFiles: "files",
		OperationBatches: "batches", OperationRerank: "rerank",
		OperationAsyncInvoke: "async_generate",
	}
	for _, profile := range domain.AllProviderProfiles() {
		manifest, ok := BuiltinProfile(profile.ID)
		if !ok {
			continue
		}
		for _, operation := range manifest.Operations {
			name, known := capabilityFor[operation]
			if !known {
				t.Errorf("%s declares operation %q, which maps to no capability", profile.ID, operation)
				continue
			}
			if !capabilityEnabled(profile.Ceiling, name) {
				t.Logf("note: %s serves %q but its ceiling does not offer %q", profile.ID, operation, name)
			}
		}
	}
}

func capabilityEnabled(c domain.ProviderCapabilities, name string) bool {
	switch name {
	case "chat":
		return c.Chat
	case "streaming":
		return c.Streaming
	case "embeddings":
		return c.Embeddings
	case "moderations":
		return c.Moderations
	case "images":
		return c.Images
	case "transcriptions":
		return c.Transcriptions
	case "speech":
		return c.Speech
	case "files":
		return c.Files
	case "batches":
		return c.Batches
	case "rerank":
		return c.Rerank
	case "async_generate":
		return c.AsyncGenerate
	default:
		return false
	}
}
