package grok

// Port of GrokEndFeatureRunRepairTests.swift (1:1 case mapping).

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRepairInsertsStubHandoffAndCoercesDirtyValidatorsPassed(t *testing.T) {
	arguments := map[string]any{
		"successState":         "success",
		"returnToOrchestrator": false,
		"featureId":            "m1-migration-runner-and-lint-baseline",
		"commitId":             "2c86843",
		"repoPath":             "/Users/josefchen/Projects/panopticon",
		"validatorsPassed":     "true persistence_required=true? No that's not a param. Just call it.",
	}

	repaired := RepairToolCall("EndFeatureRun", arguments,
		"Migration runner is in place and verified. Handing off the completed feature.")

	if !repaired.Changed {
		t.Fatalf("expected changed")
	}
	if repaired.Arguments["validatorsPassed"] != true {
		t.Errorf("validatorsPassed = %v, want true", repaired.Arguments["validatorsPassed"])
	}
	handoff, ok := repaired.Arguments["handoff"].(map[string]any)
	if !ok {
		t.Fatalf("handoff = %v, want object", repaired.Arguments["handoff"])
	}
	if handoff["salientSummary"] != "Migration runner is in place and verified. Handing off the completed feature." {
		t.Errorf("salientSummary = %v", handoff["salientSummary"])
	}
	if _, ok := handoff["discoveredIssues"]; !ok {
		t.Errorf("discoveredIssues missing")
	}
}

func TestRepairInsertsHandoffWhenValidatorsPassedAlreadyBool(t *testing.T) {
	arguments := map[string]any{
		"successState":         "success",
		"returnToOrchestrator": false,
		"featureId":            "m1-migration-runner-and-lint-baseline",
		"validatorsPassed":     true,
	}

	repaired := RepairToolCall("EndFeatureRun", arguments, "")
	if !repaired.Changed {
		t.Fatalf("expected changed")
	}
	if repaired.Arguments["validatorsPassed"] != true {
		t.Errorf("validatorsPassed = %v", repaired.Arguments["validatorsPassed"])
	}
	if _, ok := repaired.Arguments["handoff"].(map[string]any); !ok {
		t.Errorf("handoff = %v, want object", repaired.Arguments["handoff"])
	}
}

func TestRepairDefaultsMissingValidatorsPassedOnSuccess(t *testing.T) {
	arguments := map[string]any{
		"successState":         "success",
		"returnToOrchestrator": false,
		"handoff":              map[string]any{"salientSummary": "done"},
	}
	repaired := RepairToolCall("EndFeatureRun", arguments, "")
	if !repaired.Changed {
		t.Fatalf("expected changed")
	}
	if repaired.Arguments["validatorsPassed"] != true {
		t.Errorf("validatorsPassed = %v", repaired.Arguments["validatorsPassed"])
	}
}

func TestRepairLeavesCompletePayloadUnchanged(t *testing.T) {
	arguments := map[string]any{
		"successState":         "success",
		"returnToOrchestrator": true,
		"featureId":            "m1-migration-runner-and-lint-baseline",
		"validatorsPassed":     true,
		"handoff": map[string]any{
			"salientSummary":     "done",
			"whatWasImplemented": "runner",
			"whatWasLeftUndone":  "",
			"discoveredIssues":   []any{},
		},
	}
	repaired := RepairToolCall("EndFeatureRun", arguments, "")
	if repaired.Changed {
		t.Errorf("expected unchanged, notes = %v", repaired.Notes)
	}
}

func TestRepairLeavesCompleteExecuteUnchanged(t *testing.T) {
	repaired := RepairToolCall("Execute", map[string]any{"command": "ls"}, "")
	if repaired.Changed {
		t.Errorf("expected unchanged")
	}
}

func TestRepairRemapsExecuteCmdAliasToCommand(t *testing.T) {
	repaired := RepairToolCall("Execute", map[string]any{
		"cmd":     "pwd",
		"summary": "Print working directory",
	}, "")
	if !repaired.Changed {
		t.Fatalf("expected changed")
	}
	if repaired.Arguments["command"] != "pwd" {
		t.Errorf("command = %v", repaired.Arguments["command"])
	}
	if repaired.Arguments["summary"] != "Print working directory" {
		t.Errorf("summary = %v", repaired.Arguments["summary"])
	}
}

func TestRepairExtractsCommandEmbeddedInSummary(t *testing.T) {
	repaired := RepairToolCall("Execute", map[string]any{
		"summary": "Probe HexDB landing relations\ncommand\nnpx dotenv -e .env.local -- node /tmp/panopticon-probe-landing.mjs",
	}, "")
	if !repaired.Changed {
		t.Fatalf("expected changed")
	}
	if repaired.Arguments["command"] != "npx dotenv -e .env.local -- node /tmp/panopticon-probe-landing.mjs" {
		t.Errorf("command = %v", repaired.Arguments["command"])
	}
}

func TestRepairPromotesShellLikeSummaryToCommand(t *testing.T) {
	repaired := RepairToolCall("Execute", map[string]any{"summary": "pwd"}, "")
	if !repaired.Changed {
		t.Fatalf("expected changed")
	}
	if repaired.Arguments["command"] != "pwd" {
		t.Errorf("command = %v", repaired.Arguments["command"])
	}
}

func TestRepairDoesNotInventCommandFromEnglishSummary(t *testing.T) {
	repaired := RepairToolCall("Execute", map[string]any{"summary": "List current directory contents"}, "")
	if repaired.Changed {
		t.Errorf("expected unchanged")
	}
	if _, ok := repaired.Arguments["command"]; ok {
		t.Errorf("command = %v, want absent", repaired.Arguments["command"])
	}
}

func TestRepairDoesNotInferExecuteFromSummaryAlone(t *testing.T) {
	_, ok := RepairArgumentsJSONInferred(`{"summary":"pwd"}`, "")
	if ok {
		t.Errorf("expected no inference")
	}
}

func TestRepairExtractsFencedCommandFromAssistantText(t *testing.T) {
	repaired := RepairToolCall("Execute",
		map[string]any{"summary": "Probe HexDB for 23 landing relations"},
		"Run this exact command:\n\n```\nnpx dotenv -e .env.local -- node /tmp/panopticon-probe-landing.mjs\n```\n")
	if !repaired.Changed {
		t.Fatalf("expected changed")
	}
	if repaired.Arguments["command"] != "npx dotenv -e .env.local -- node /tmp/panopticon-probe-landing.mjs" {
		t.Errorf("command = %v", repaired.Arguments["command"])
	}
}

func TestRepairRemapsReadPathAlias(t *testing.T) {
	repaired := RepairToolCall("Read", map[string]any{"path": "/tmp/a.swift"}, "")
	if !repaired.Changed {
		t.Fatalf("expected changed")
	}
	if repaired.Arguments["file_path"] != "/tmp/a.swift" {
		t.Errorf("file_path = %v", repaired.Arguments["file_path"])
	}
}

func TestRepairRemapsGrepRegexAlias(t *testing.T) {
	repaired := RepairToolCall("Grep", map[string]any{"regex": "uppercase"}, "")
	if !repaired.Changed {
		t.Fatalf("expected changed")
	}
	if repaired.Arguments["pattern"] != "uppercase" {
		t.Errorf("pattern = %v", repaired.Arguments["pattern"])
	}
}

func TestRepairExecuteInResponsesAPIFunctionCall(t *testing.T) {
	body := `{"output":[{"type":"function_call","name":"Execute","arguments":"{\"summary\":\"Print working directory\",\"cmd\":\"pwd\"}"}]}`
	rewritten, ok := RepairJSONBody(body, "")
	if !ok {
		t.Fatalf("expected rewrite")
	}
	root := sanitizeTestJSON(t, rewritten)
	output := root["output"].([]any)
	argsJSON, ok := output[0].(map[string]any)["arguments"].(string)
	if !ok {
		t.Fatalf("arguments = %v, want string", output[0].(map[string]any)["arguments"])
	}
	args := sanitizeTestJSON(t, argsJSON)
	if args["command"] != "pwd" {
		t.Errorf("command = %v", args["command"])
	}
}

func TestRepairResponsesAPIFunctionCall(t *testing.T) {
	body := `{"output":[{"type":"function_call","name":"EndFeatureRun","arguments":"{\"successState\":\"success\",\"featureId\":\"m1-migration-runner-and-lint-baseline\",\"validatorsPassed\":true}"}]}`
	rewritten, ok := RepairJSONBody(body, "")
	if !ok {
		t.Fatalf("expected rewrite")
	}
	root := sanitizeTestJSON(t, rewritten)
	output := root["output"].([]any)
	argsJSON, ok := output[0].(map[string]any)["arguments"].(string)
	if !ok {
		t.Fatalf("arguments = %v, want string", output[0].(map[string]any)["arguments"])
	}
	args := sanitizeTestJSON(t, argsJSON)
	if _, ok := args["handoff"].(map[string]any); !ok {
		t.Errorf("handoff = %v, want object", args["handoff"])
	}
	if args["validatorsPassed"] != true {
		t.Errorf("validatorsPassed = %v", args["validatorsPassed"])
	}
}

func TestRepairChatCompletionToolCall(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":"Handing off.","tool_calls":[{"id":"call_1","type":"function","function":{"name":"EndFeatureRun","arguments":"{\"successState\":\"success\",\"validatorsPassed\":true}"}}]}}]}`
	rewritten, ok := RepairJSONBody(body, "")
	if !ok {
		t.Fatalf("expected rewrite")
	}
	if !strings.Contains(rewritten, "handoff") {
		t.Errorf("missing handoff: %s", rewritten)
	}
	if !strings.Contains(rewritten, "salientSummary") {
		t.Errorf("missing salientSummary: %s", rewritten)
	}
}

func TestRepairResponsesSSEDoneEvent(t *testing.T) {
	sse := "event: response.function_call_arguments.delta\ndata: {\"delta\":\"{\\\"successState\\\":\"}\n\nevent: response.function_call_arguments.done\ndata: {\"arguments\":\"{\\\"successState\\\":\\\"success\\\",\\\"featureId\\\":\\\"m1-x\\\",\\\"validatorsPassed\\\":true}\"}\n\n"

	rewritten, ok := RepairSSE(sse)
	if !ok {
		t.Fatalf("expected rewrite")
	}
	if strings.Contains(rewritten, "function_call_arguments.delta") {
		t.Errorf("delta events should be dropped: %s", rewritten)
	}
	if !strings.Contains(rewritten, "handoff") {
		t.Errorf("missing handoff: %s", rewritten)
	}
}

// Extra: number fidelity in repaired arguments must round-trip.
func TestRepairArgumentsPreserveLargeIntegers(t *testing.T) {
	out, ok := RepairArgumentsJSON("EndFeatureRun",
		`{"successState":"success","featureId":"x","validatorsPassed":true,"lineNumber":9007199254740993}`, "")
	if !ok {
		t.Fatalf("expected repair")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(raw["handoff"], &args); err != nil {
		t.Fatalf("unmarshal handoff: %v", err)
	}
	if got := string(args["salientSummary"]); got != `"Grok omitted the required handoff object. DroidProxy inserted a stub so EndFeatureRun could close."` {
		t.Errorf("salientSummary = %s", got)
	}
}
