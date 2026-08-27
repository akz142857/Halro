package semantic_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// This is an executable architecture rule. Keeping it in the normal Go test
// graph makes local tests and CI reject protocol/provider leakage together.
func TestSemanticPackageHasNoProtocolOrProviderDependencies(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	allowedImports := map[string]struct{}{"bytes": {}, "encoding/json": {}, "errors": {}, "fmt": {}}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Clean(entry.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imported := range parsed.Imports {
			path := strings.Trim(imported.Path.Value, `"`)
			if _, allowed := allowedImports[path]; !allowed {
				t.Errorf("%s imports non-allowlisted dependency %s", entry.Name(), path)
			}
		}
	}
}

// This allowlist is the public, provider-neutral canonical schema. Unlike a
// blacklist of known provider names, it makes every newly exported struct or
// field an explicit architecture decision reviewed together with mappings.
func TestExportedCanonicalSchemaMatchesReviewedSnapshot(t *testing.T) {
	want := map[string][]string{
		// ToolError was reviewed in as provider-neutral: "this tool result is a
		// failure" is a property of the conversation, not of one vendor's wire
		// form. It is carried rather than dropped so that the profiles which
		// cannot express it declare the loss instead of feeding a model a failed
		// result dressed as a successful one.
		"Citation":         {"URL=url", "Title=title", "StartIndex=start_index", "EndIndex=end_index"},
		"Content":          {"Kind=kind", "Text=text", "URL=url", "Detail=detail", "CallID=call_id", "Name=name", "Arguments=arguments", "ToolError=tool_error", "Status=status", "Citations=citations"},
		"Message":          {"Role=role", "Name=name", "Content=content"},
		"Source":           {"ProfileID=profile_id", "ProfileRevision=profile_revision"},
		"Requirements":     {"Streaming=streaming", "StreamUsage=stream_usage", "Tools=tools", "ParallelTools=parallel_tools", "Vision=vision", "FetchedImage=fetched_image", "JSONObject=json_object", "StructuredOutputs=structured_outputs", "DeveloperRole=developer_role", "Reasoning=reasoning", "Seed=seed", "MultipleCandidates=multiple_candidates", "EndUserReference=end_user_reference", "ProviderExecutedTools=provider_executed_tools"},
		"Tool":             {"Name=name", "Description=description", "Schema=schema", "Execution=execution"},
		"ToolChoice":       {"Mode=mode", "Name=name"},
		"OutputFormat":     {"Kind=kind", "Name=name", "Description=description", "Schema=schema", "Strict=strict"},
		"GenerateRequest":  {"Operation=operation", "Source=source", "Mode=mode", "RequestedModel=requested_model", "Messages=messages", "Tools=tools", "ToolChoice=tool_choice", "ParallelTools=parallel_tools", "OutputFormat=output_format", "Temperature=temperature", "TopP=top_p", "VisibleOutputTokenLimit=visible_output_token_limit", "CompletionTokenLimit=completion_token_limit", "Candidates=candidates", "Stop=stop", "Seed=seed", "ReasoningEffort=reasoning_effort", "EndUserRef=end_user_ref", "Stream=stream", "IncludeUsage=include_usage", "Requirements=requirements"},
		"EmbeddingRequest": {"Operation=operation", "Source=source", "Mode=mode", "RequestedModel=requested_model", "Input=input", "Encoding=encoding", "Dimensions=dimensions", "EndUserRef=end_user_ref", "Requirements=requirements"},
		"Usage":            {"InputTokens=input_tokens", "CachedInputTokens=cached_input_tokens", "CacheWriteInputTokens=cache_write_input_tokens", "OutputTokens=output_tokens", "ReasoningTokens=reasoning_tokens", "AudioTokens=audio_tokens", "TotalTokens=total_tokens", "Source=source"},
		"GenerateChoice":   {"Index=index", "Message=message", "Termination=termination", "NativeTermination=native_termination"},
		"GenerateResult":   {"ID=id", "Created=created", "Model=model", "Choices=choices", "Usage=usage", "ProviderRequestID=provider_request_id", "ProviderID=provider_id", "DeploymentID=deployment_id", "Translation=translation", "MappingRevision=mapping_revision"},
		"Embedding":        {"Index=index", "Value=value"},
		"EmbeddingResult":  {"Model=model", "Data=data", "Usage=usage", "ProviderRequestID=provider_request_id", "ProviderID=provider_id", "DeploymentID=deployment_id", "Translation=translation", "MappingRevision=mapping_revision"},
		"Event":            {"Kind=kind", "ID=id", "Created=created", "Model=model", "Outputs=outputs", "Usage=usage", "Translation=translation", "MappingRevision=mapping_revision"},
		"OutputDelta":      {"Index=index", "Role=role", "Content=content", "Termination=termination", "NativeTermination=native_termination"},
		"ContentDelta":     {"Kind=kind", "Text=text", "ToolIndex=tool_index", "CallID=call_id", "Name=name", "ArgumentsFragment=arguments_fragment"},
	}
	got := exportedStructFields(t)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exported canonical schema changed without snapshot review\nwant: %#v\n got: %#v", want, got)
	}
}

func exportedStructFields(t *testing.T) map[string][]string {
	t.Helper()
	result := map[string][]string{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Clean(entry.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range parsed.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.TYPE {
				continue
			}
			for _, specification := range general.Specs {
				typeSpec := specification.(*ast.TypeSpec)
				structure, ok := typeSpec.Type.(*ast.StructType)
				if !ok || !typeSpec.Name.IsExported() {
					continue
				}
				for _, field := range structure.Fields.List {
					jsonName := ""
					if field.Tag != nil {
						rawTag, unquoteErr := strconv.Unquote(field.Tag.Value)
						if unquoteErr != nil {
							t.Fatal(unquoteErr)
						}
						jsonName = strings.Split(reflect.StructTag(rawTag).Get("json"), ",")[0]
					}
					for _, name := range field.Names {
						if name.IsExported() {
							result[typeSpec.Name.Name] = append(result[typeSpec.Name.Name], name.Name+"="+jsonName)
						}
					}
				}
			}
		}
	}
	return result
}
