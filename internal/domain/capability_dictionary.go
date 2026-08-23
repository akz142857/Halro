package domain

import "slices"

// capabilityField binds a capability's dictionary name to the member that holds
// it. This slice is the dictionary: its order is the stable order every name
// list and projection uses, and its entries are the only place a capability's
// name and its storage are written down together.
//
// It replaces six separate enumerations of the same set — a name list, a by-name
// read, a by-name write, an evidence projection, an evidence validation, and a
// subset check — each of which had to be edited to add a capability and none of
// which failed when it was not. Two of them were in fact missed while adding
// fetched_image, and both surfaced as working features that quietly did the
// wrong thing: a capability recorded as unsupported while it was enabled, and a
// connection summary that dropped it.
//
// The accessor returns a pointer so one declaration serves both directions. A
// pair of get/set closures would be two places to get a member wrong.
var capabilityFields = []struct {
	Name  string
	Value func(*ProviderCapabilities) *bool
}{
	{"chat", func(c *ProviderCapabilities) *bool { return &c.Chat }},
	{"streaming", func(c *ProviderCapabilities) *bool { return &c.Streaming }},
	{"embeddings", func(c *ProviderCapabilities) *bool { return &c.Embeddings }},
	{"tools", func(c *ProviderCapabilities) *bool { return &c.Tools }},
	{"vision", func(c *ProviderCapabilities) *bool { return &c.Vision }},
	{"fetched_image", func(c *ProviderCapabilities) *bool { return &c.FetchedImage }},
	{"json_mode", func(c *ProviderCapabilities) *bool { return &c.JSONMode }},
	{"developer_role", func(c *ProviderCapabilities) *bool { return &c.DeveloperRole }},
	{"reasoning", func(c *ProviderCapabilities) *bool { return &c.Reasoning }},
	{"stream_usage", func(c *ProviderCapabilities) *bool { return &c.StreamUsage }},
	{"provider_executed_tools", func(c *ProviderCapabilities) *bool { return &c.ProviderExecutedTools }},
	{"moderations", func(c *ProviderCapabilities) *bool { return &c.Moderations }},
	{"images", func(c *ProviderCapabilities) *bool { return &c.Images }},
	{"transcriptions", func(c *ProviderCapabilities) *bool { return &c.Transcriptions }},
	{"speech", func(c *ProviderCapabilities) *bool { return &c.Speech }},
	{"files", func(c *ProviderCapabilities) *bool { return &c.Files }},
	{"batches", func(c *ProviderCapabilities) *bool { return &c.Batches }},
	{"rerank", func(c *ProviderCapabilities) *bool { return &c.Rerank }},
	{"async_generate", func(c *ProviderCapabilities) *bool { return &c.AsyncGenerate }},
}

var capabilityNames = func() []string {
	names := make([]string, 0, len(capabilityFields))
	for _, field := range capabilityFields {
		names = append(names, field.Name)
	}
	return names
}()

// CapabilityValue reports one capability by its dictionary name. The second
// result separates "this capability is off" from "there is no such capability",
// which a caller comparing against a stored or transmitted name has to know.
func CapabilityValue(capabilities ProviderCapabilities, name string) (bool, bool) {
	for _, field := range capabilityFields {
		if field.Name == name {
			return *field.Value(&capabilities), true
		}
	}
	return false, false
}

// SetCapability writes one capability by name and reports whether the name is
// one the dictionary carries. A write to an unknown name is refused rather than
// dropped: it means a caller and this dictionary disagree about what exists.
func SetCapability(capabilities *ProviderCapabilities, name string, value bool) bool {
	for _, field := range capabilityFields {
		if field.Name == name {
			*field.Value(capabilities) = value
			return true
		}
	}
	return false
}

// EnabledCapabilityNames lists what is on, in dictionary order.
func EnabledCapabilityNames(capabilities ProviderCapabilities) []string {
	names := make([]string, 0, len(capabilityFields))
	for _, field := range capabilityFields {
		if *field.Value(&capabilities) {
			names = append(names, field.Name)
		}
	}
	return names
}

// CapabilityDifference lists what is on in after and off in before, in
// dictionary order. Limits are excluded: they narrow which requests fit rather
// than whether a target is a candidate at all.
func CapabilityDifference(before, after ProviderCapabilities) []string {
	names := make([]string, 0, len(capabilityFields))
	for _, field := range capabilityFields {
		if *field.Value(&after) && !*field.Value(&before) {
			names = append(names, field.Name)
		}
	}
	return names
}

// IsCapabilityName reports whether the dictionary carries this name.
func IsCapabilityName(name string) bool { return slices.Contains(capabilityNames, name) }
