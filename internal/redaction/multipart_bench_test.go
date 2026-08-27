package redaction

import (
	"fmt"
	"testing"

	"github.com/akz142857/Halro/internal/semantic"
)

// The unary path now redacts the semantic request rather than one facade's wire
// bytes, so its cost scales with content parts rather than with messages. The
// range that made that change removed two whole-message traversals — a second
// translation after redaction, and the Responses/Chat round trip — and added
// per-part work in their place, which a single-part sample cannot see either way.
//
// This is a HEAD-only baseline: v0.3.0 has no semantic traversal to compare
// against, only a wire-bytes one, so a same-name benchmark on both trees would
// be measuring two different things.
func BenchmarkOutboundGenerateResultManyParts(b *testing.B) {
	for _, parts := range []int{1, 8, 64} {
		b.Run(fmt.Sprintf("parts=%d", parts), func(b *testing.B) {
			engine := NewDefault()
			content := make([]semantic.Content, 0, parts)
			for index := range parts {
				content = append(content, semantic.Content{
					Kind: semantic.ContentText,
					Text: fmt.Sprintf("part %d: contact 13800138000 or mail nobody@example.test", index),
				})
			}
			result := semantic.GenerateResult{
				ID: "resp_1", Model: "chat", MappingRevision: 1, Translation: semantic.TranslationNone,
				Choices: []semantic.GenerateChoice{{Index: 0, Message: semantic.Message{
					Role: semantic.RoleAssistant, Content: content,
				}}},
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := engine.ProcessOutboundGenerateResult("", result); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
