package main

import (
	"strings"
	"testing"
)

func TestScrubOutputRemovesEveryConfiguredValue(t *testing.T) {
	values := map[string]string{"API_KEY": "secret-key", "BASE_URL": "https://provider.example"}
	output := scrubOutput("secret-key at https://provider.example", values)
	if strings.Contains(output, "secret-key") || strings.Contains(output, "provider.example") {
		t.Fatalf("configured value leaked: %q", output)
	}
}

func TestSmokeEnvironmentDoesNotExposeOtherMatrixProfiles(t *testing.T) {
	t.Setenv("HALRO_MATRIX_DEEPSEEK_API_KEY", "other-secret")
	environment := smokeEnvironment(gaProfiles[0], map[string]string{
		"BASE_URL": "https://api.openai.com/v1", "API_KEY": "selected-secret", "MODEL": "model", "EMBEDDING_MODEL": "embedding",
	})
	joined := strings.Join(environment, "\n")
	if strings.Contains(joined, "other-secret") || !strings.Contains(joined, "HALRO_SMOKE_API_KEY=selected-secret") ||
		!strings.Contains(joined, "HALRO_SMOKE_CAPABILITY_DETECTION=1") {
		t.Fatalf("unexpected child environment: %s", scrubOutput(joined, map[string]string{"API_KEY": "selected-secret"}))
	}
}
