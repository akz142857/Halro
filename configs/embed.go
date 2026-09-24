// Package configs embeds the annotated operator-facing configuration
// reference, so the running binary does not depend on a source checkout being
// present and the file administrators copy stays the one source of the
// console's field metadata.
package configs

import _ "embed"

// ExampleYAML is the annotated configuration reference: every key the
// configuration type can produce, with a bilingual title and description.
//
// "Every key" is held to by a test rather than by this sentence. It was not
// true for a long time — the whole key_slots half of storage.master_key was
// absent while this comment already called the file complete — because the two
// gates that compared it against Default() both compared *decoded values*, and
// a key whose default is the zero value reads identically whether it is
// documented or missing. TestTheConfigurationReferenceDocumentsEveryKey
// compares keys instead, enumerated by reflection over the configuration type,
// which is the only list that cannot itself drift.
//
// Keys mutually exclusive with another mode are documented commented out. They
// have to be: validation refuses a file that sets both storage.master_key.file
// and the key_slots fields, so a reference carrying all of them live would be
// a reference that does not load.
//
// The console reads only titles and descriptions from here; the values it
// shows come from the instance's own effective configuration. The values
// written here are still a claim about the defaults, and
// TestConfigReferenceValuesMatchTheShippedDefaults holds them to it.
//
//go:embed config.example.yaml
var ExampleYAML []byte
