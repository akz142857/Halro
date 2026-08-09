// Command provider-matrix executes the opt-in real-account smoke contract for
// every GA Provider profile and emits one secret-scrubbed evidence document.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

type profile struct {
	Name     string
	Prefix   string
	Required []string
}

var gaProfiles = []profile{
	{Name: "openai", Prefix: "OPENAI", Required: []string{"BASE_URL", "API_KEY", "MODEL", "EMBEDDING_MODEL"}},
	{Name: "azure_openai", Prefix: "AZURE_OPENAI", Required: []string{"BASE_URL", "API_KEY", "MODEL", "EMBEDDING_MODEL", "API_VERSION"}},
	{Name: "deepseek", Prefix: "DEEPSEEK", Required: []string{"BASE_URL", "API_KEY", "MODEL"}},
	{Name: "openai_compatible", Prefix: "OPENAI_COMPATIBLE", Required: []string{"BASE_URL", "API_KEY", "MODEL", "EMBEDDING_MODEL"}},
}

type result struct {
	Profile    string        `json:"profile"`
	Status     string        `json:"status"`
	Duration   time.Duration `json:"duration_ns"`
	Missing    []string      `json:"missing,omitempty"`
	SafeOutput string        `json:"safe_output,omitempty"`
}

type report struct {
	Commit     string    `json:"commit"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Passed     bool      `json:"passed"`
	Results    []result  `json:"results"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "provider-matrix:", err)
		os.Exit(1)
	}
}

func run() error {
	commit := flag.String("commit", "", "exact 40-character RC commit")
	output := flag.String("output", "provider-matrix.json", "new evidence file")
	timeout := flag.Duration("timeout", 2*time.Minute, "timeout per Provider profile")
	flag.Parse()
	if !commitPattern.MatchString(*commit) {
		return errors.New("-commit must be the exact lowercase 40-character commit")
	}
	if *timeout <= 0 || *timeout > 10*time.Minute {
		return errors.New("-timeout must be positive and at most 10 minutes")
	}
	started := time.Now().UTC()
	results := make([]result, 0, len(gaProfiles))
	passed := true
	for _, item := range gaProfiles {
		current := executeProfile(item, *timeout)
		results = append(results, current)
		if current.Status != "passed" {
			passed = false
		}
	}
	document := report{Commit: *commit, StartedAt: started, FinishedAt: time.Now().UTC(), Passed: passed, Results: results}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(*output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create fresh evidence file: %w", err)
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		_ = file.Close()
		return fmt.Errorf("write evidence file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close evidence file: %w", err)
	}
	fmt.Printf("provider matrix passed=%t evidence=%s\n", passed, *output)
	if !passed {
		return errors.New("one or more GA Provider profiles did not pass")
	}
	return nil
}

func executeProfile(item profile, timeout time.Duration) result {
	values, missing := profileValues(item)
	if len(missing) > 0 {
		return result{Profile: item.Name, Status: "missing_credentials", Missing: missing}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "test", "./internal/provider/openai", "-run", "^TestRealProviderSmoke$", "-count=1", "-v")
	command.Env = smokeEnvironment(item, values)
	started := time.Now()
	output, err := command.CombinedOutput()
	duration := time.Since(started)
	safeOutput := scrubOutput(string(output), values)
	if len(safeOutput) > 16<<10 {
		safeOutput = safeOutput[len(safeOutput)-(16<<10):]
	}
	status := "passed"
	if ctx.Err() != nil {
		status = "timeout"
	} else if err != nil {
		status = "failed"
	}
	return result{Profile: item.Name, Status: status, Duration: duration, SafeOutput: safeOutput}
}

func profileValues(item profile) (map[string]string, []string) {
	values := make(map[string]string, len(item.Required))
	missing := make([]string, 0)
	for _, suffix := range item.Required {
		name := "HALRO_MATRIX_" + item.Prefix + "_" + suffix
		value := os.Getenv(name)
		if value == "" {
			missing = append(missing, name)
			continue
		}
		values[suffix] = value
	}
	return values, missing
}

func smokeEnvironment(item profile, values map[string]string) []string {
	environment := make([]string, 0, len(os.Environ())+8)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "HALRO_MATRIX_") || strings.HasPrefix(entry, "HALRO_SMOKE_") || strings.HasPrefix(entry, "HALRO_REAL_PROVIDER_SMOKE=") {
			continue
		}
		environment = append(environment, entry)
	}
	environment = append(environment,
		"HALRO_REAL_PROVIDER_SMOKE=1",
		"HALRO_SMOKE_CAPABILITY_DETECTION=1",
		"HALRO_SMOKE_PROFILE="+item.Name,
		"HALRO_SMOKE_BASE_URL="+values["BASE_URL"],
		"HALRO_SMOKE_API_KEY="+values["API_KEY"],
		"HALRO_SMOKE_MODEL="+values["MODEL"],
	)
	for _, suffix := range []string{"EMBEDDING_MODEL", "API_VERSION"} {
		if value := values[suffix]; value != "" {
			environment = append(environment, "HALRO_SMOKE_"+suffix+"="+value)
		}
	}
	return environment
}

func scrubOutput(output string, values map[string]string) string {
	for _, value := range values {
		if value != "" {
			output = strings.ReplaceAll(output, value, "[REDACTED]")
		}
	}
	return output
}
