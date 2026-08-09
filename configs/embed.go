// Package configs embeds the annotated operator-facing configuration reference.
// The running binary therefore does not depend on a source checkout being
// present, while the file administrators copy remains the UI metadata source.
package configs

import _ "embed"

// ExampleYAML is the complete annotated configuration reference.
//
//go:embed config.example.yaml
var ExampleYAML []byte
