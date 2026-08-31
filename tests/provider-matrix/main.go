// Command provider-matrix executes the opt-in real-account smoke contract for
// every GA Provider profile and emits one secret-scrubbed evidence document.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

type profile struct {
	Name     string
	Prefix   string
	Required []string
	// Package and Optional exist because the Beta profiles are not variants of
	// the GA ones: they live in their own adapter package and take settings the
	// GA smoke has no concept of, such as which wire protocol and which Bedrock
	// Project the run addresses.
	Package string
	// Test names the smoke to run when it is not the package's
	// TestRealProviderSmoke. It exists for the same reason Package does: a
	// platform whose smoke does not sit where the convention puts it should say
	// so here rather than have the runner guess.
	//
	// MiniMax is the case. It serves three wire shapes from two adapter packages
	// that each already have a TestRealProviderSmoke of their own, built around a
	// different set of assertions, so folding MiniMax into them would make one
	// function serve two unrelated purposes.
	Test     string
	Optional []string
}

// betaProfiles never take part in the GA release gate. They run only with
// -include-beta and their results are reported separately, so a Beta pass can
// never be mistaken for evidence that a GA profile was exercised.
//
// One Bedrock Mantle run proves one cell: commit × region × profile × exact
// model × authentication × project mode. Running it three times for the three
// wire profiles is three cells, not "Mantle passed".
var betaProfiles = []profile{
	{
		Name: "bedrock_mantle", Prefix: "BEDROCK_MANTLE", Package: "./internal/provider/bedrockmantle",
		Required: []string{"BASE_URL", "API_KEY", "MODEL", "MANTLE_PROFILE"},
		Optional: []string{"BEDROCK_PROJECT_ID"},
	},
	// MiniMax is two rows on one credential, because one connection serves three
	// wire shapes and the OpenAI-shaped two live in a different adapter package
	// from the Anthropic one. They share a prefix so an operator configures one
	// account, and they are separate rows because each is one package's run.
	//
	// The region is an axis here in a way it is not for any other profile: the
	// same contract is served from api.minimax.io and api.minimaxi.com on keys
	// that are not interchangeable, and Halro serves both from one profile group.
	// A run therefore covers one region, and the other stays not measured — BASE_URL
	// is what says which.
	{
		Name: "minimax", Prefix: "MINIMAX", Package: "./internal/provider/openai",
		Test:     "TestRealMiniMaxSmoke",
		Required: []string{"BASE_URL", "API_KEY", "MODEL"},
		Optional: []string{"M2_MODEL"},
	},
	{
		Name: "minimax_anthropic", Prefix: "MINIMAX", Package: "./internal/provider/anthropic",
		Test:     "TestRealMiniMaxAnthropicRouteSmoke",
		Required: []string{"BASE_URL", "API_KEY", "MODEL"},
	},
}

var gaProfiles = []profile{
	{Name: "openai", Prefix: "OPENAI", Required: []string{"BASE_URL", "API_KEY", "MODEL", "EMBEDDING_MODEL"}},
	// Anthropic has its own adapter package because it is the only GA profile
	// with two execution modes. A run has to prove both the verbatim native path
	// and the re-authored portable one; neither says anything about the other.
	{Name: "anthropic", Prefix: "ANTHROPIC", Package: "./internal/provider/anthropic", Required: []string{"BASE_URL", "API_KEY", "MODEL"}},
	{Name: "azure_openai", Prefix: "AZURE_OPENAI", Required: []string{"BASE_URL", "API_KEY", "MODEL", "EMBEDDING_MODEL", "API_VERSION"}},
	{Name: "deepseek", Prefix: "DEEPSEEK", Required: []string{"BASE_URL", "API_KEY", "MODEL"}},
	{Name: "openai_compatible", Prefix: "OPENAI_COMPATIBLE", Required: []string{"BASE_URL", "API_KEY", "MODEL", "EMBEDDING_MODEL"}},
}

type result struct {
	Profile  string        `json:"profile"`
	Status   string        `json:"status"`
	Duration time.Duration `json:"duration_ns"`
	Missing  []string      `json:"missing,omitempty"`
	// Tier separates a release-gating GA result from a Beta one that gates
	// nothing. Without it the two are one list and a reader has to know the
	// profile names to tell them apart.
	Tier string `json:"tier"`
	// Region, WireProfile, Authentication and ProjectMode are the axes a Mantle
	// result is only valid along. TargetDigest binds the exact model without
	// storing it: the evidence is shared, and an account's model entitlements
	// are not ours to publish.
	Region         string `json:"region,omitempty"`
	WireProfile    string `json:"wire_profile,omitempty"`
	Authentication string `json:"authentication,omitempty"`
	ProjectMode    string `json:"project_mode,omitempty"`
	TargetDigest   string `json:"target_digest,omitempty"`
	SafeOutput     string `json:"safe_output,omitempty"`
}

type report struct {
	Commit     string    `json:"commit"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	// Passed is the GA release gate and counts GA results only. Beta results
	// appear in Results with tier="beta" and never move it.
	Passed  bool     `json:"passed"`
	Results []result `json:"results"`
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
	includeBeta := flag.Bool("include-beta", false, "also run Beta profile smokes; they never affect the release gate")
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
		current.Tier = "ga"
		results = append(results, current)
		if current.Status != "passed" {
			passed = false
		}
	}
	if *includeBeta {
		for _, item := range betaProfiles {
			current := executeProfile(item, *timeout)
			current.Tier = "beta"
			results = append(results, current)
			// Deliberately not folded into `passed`. A Beta profile that fails
			// is not a release blocker, and one that passes is evidence about
			// one cell, not a gate.
		}
	} else {
		// Silence would read as "there was nothing to run".
		for _, item := range betaProfiles {
			results = append(results, result{Profile: item.Name, Tier: "beta", Status: "not_run"})
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
	pkg := item.Package
	if pkg == "" {
		pkg = "./internal/provider/openai"
	}
	test := item.Test
	if test == "" {
		test = "TestRealProviderSmoke"
	}
	command := exec.CommandContext(ctx, "go", "test", pkg, "-run", "^"+test+"$", "-count=1", "-v")
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
	current := result{Profile: item.Name, Status: status, Duration: duration, SafeOutput: safeOutput}
	describeCell(&current, item, values)
	return current
}

func profileValues(item profile) (map[string]string, []string) {
	values := make(map[string]string, len(item.Required)+len(item.Optional))
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
	for _, suffix := range item.Optional {
		if value := os.Getenv("HALRO_MATRIX_" + item.Prefix + "_" + suffix); value != "" {
			values[suffix] = value
		}
	}
	return values, missing
}

// describeCell records which cell of the matrix a Bedrock Mantle run covers.
//
// The exact model is bound rather than stored: the digest lets a later reader
// confirm that two runs used the same target, or that a claimed target matches
// a custody record they hold, without this shared file naming an account's
// model entitlements. Region comes from the endpoint host, which is the only
// place it exists on this surface.
func describeCell(current *result, item profile, values map[string]string) {
	if strings.HasPrefix(item.Name, "minimax") {
		// Which of MiniMax's two regional hosts this run covered. A pass on one is
		// not evidence for the other: the keys are not interchangeable and nothing
		// with a credential attached has been compared across them.
		current.Region = minimaxRegion(values["BASE_URL"])
		current.Authentication = "bearer_static"
		return
	}
	if item.Name != "bedrock_mantle" {
		return
	}
	current.Region = mantleRegion(values["BASE_URL"])
	current.WireProfile = values["MANTLE_PROFILE"]
	current.Authentication = "bedrock_api_key"
	current.ProjectMode = "account_default"
	// The smoke normalises `default` and whitespace to the account default, so
	// classifying on the raw value would claim a matrix cell the run never
	// covered — and the digest binds ProjectMode, so the claim would be sealed.
	if domain.NormalizeBedrockProjectID(values["BEDROCK_PROJECT_ID"]) != "" {
		current.ProjectMode = "explicit_project"
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{
		current.Region, current.WireProfile, current.Authentication, current.ProjectMode, values["MODEL"],
	}, "\x00")))
	current.TargetDigest = "sha256:" + hex.EncodeToString(digest[:])
}

// mantleRegion reads the region out of bedrock-mantle.<region>.api.aws. An
// unrecognised host yields an empty region rather than a guess, because a wrong
// region on an evidence record is worse than a missing one.
func mantleRegion(baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	if !strings.HasPrefix(host, "bedrock-mantle.") || !strings.HasSuffix(host, ".api.aws") {
		return ""
	}
	region := strings.TrimSuffix(strings.TrimPrefix(host, "bedrock-mantle."), ".api.aws")
	if strings.Contains(region, ".") {
		return ""
	}
	return region
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
	for _, suffix := range []string{"EMBEDDING_MODEL", "API_VERSION", "MANTLE_PROFILE", "BEDROCK_PROJECT_ID", "M2_MODEL"} {
		if value := values[suffix]; value != "" {
			environment = append(environment, "HALRO_SMOKE_"+suffix+"="+value)
		}
	}
	return environment
}

// minimaxRegion names the account region a base URL addresses. The two hosts
// differ by one letter, which is exactly why the evidence should not leave it to
// a reader to spot.
func minimaxRegion(baseURL string) string {
	host := strings.ToLower(baseURL)
	switch {
	case strings.Contains(host, "api.minimaxi.com"):
		return "mainland"
	case strings.Contains(host, "api.minimax.io"):
		return "international"
	default:
		return "unrecognised"
	}
}

func scrubOutput(output string, values map[string]string) string {
	for _, value := range values {
		if value != "" {
			output = strings.ReplaceAll(output, value, "[REDACTED]")
		}
	}
	return output
}
