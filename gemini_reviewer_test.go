package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type geminiTestInvocation struct {
	Name             string
	Args             []string
	PromptPath       string
	PolicyPath       string
	SettingsPath     string
	Result           string
	FailureDetail    string
	Delay            string
	WorkingDirectory string
}

func newGeminiTestCommand(t *testing.T, model string, structured any, failureDetail string) (commandContextFunc, *geminiTestInvocation) {
	t.Helper()
	captureDirectory := t.TempDir()
	promptPath := filepath.Join(captureDirectory, "prompt.txt")
	policyPath := filepath.Join(captureDirectory, "policy.toml")
	settingsPath := filepath.Join(captureDirectory, "settings.json")
	structuredJSON, err := json.Marshal(structured)
	if err != nil {
		t.Fatal(err)
	}
	responseJSON, err := json.Marshal(string(structuredJSON))
	if err != nil {
		t.Fatal(err)
	}
	result := fmt.Sprintf(`{
		"session_id":"test","response":%s,
		"stats":{"models":{%q:{"tokens":{
			"input":90,"prompt":100,"candidates":20,"total":130,
			"cached":10,"thoughts":5,"tool":5
		}}}}
	}`, responseJSON, model)
	invocation := &geminiTestInvocation{
		PromptPath: promptPath, PolicyPath: policyPath, SettingsPath: settingsPath,
		Result: result, FailureDetail: failureDetail,
	}
	command := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		invocation.Name = name
		invocation.Args = append([]string(nil), args...)
		helperArgs := []string{"-test.run=^TestGeminiHelperProcess$", "--"}
		helperArgs = append(helperArgs, args...)
		cmd := exec.CommandContext(ctx, os.Args[0], helperArgs...)
		cmd.Env = append(os.Environ(),
			"AIR_GEMINI_HELPER=1",
			"AIR_GEMINI_HELPER_PROMPT="+promptPath,
			"AIR_GEMINI_HELPER_POLICY="+policyPath,
			"AIR_GEMINI_HELPER_SETTINGS="+settingsPath,
			"AIR_GEMINI_HELPER_RESULT="+invocation.Result,
			"AIR_GEMINI_HELPER_FAILURE="+failureDetail,
			"AIR_GEMINI_HELPER_DELAY="+invocation.Delay,
		)
		return cmd
	}
	return command, invocation
}

func TestGeminiHelperProcess(t *testing.T) {
	if os.Getenv("AIR_GEMINI_HELPER") != "1" {
		return
	}
	separator := slices.Index(os.Args, "--")
	if separator < 0 {
		fmt.Fprintln(os.Stderr, "missing helper argument separator")
		os.Exit(90)
	}
	args := os.Args[separator+1:]
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(91)
	}
	if err := os.WriteFile(os.Getenv("AIR_GEMINI_HELPER_PROMPT"), prompt, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(92)
	}
	for source, destination := range map[string]string{
		testArgumentValue(args, "--policy"):          os.Getenv("AIR_GEMINI_HELPER_POLICY"),
		os.Getenv("GEMINI_CLI_SYSTEM_SETTINGS_PATH"): os.Getenv("AIR_GEMINI_HELPER_SETTINGS"),
	} {
		contents, err := os.ReadFile(source)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(93)
		}
		if err := os.WriteFile(destination, contents, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(94)
		}
	}
	if delay := os.Getenv("AIR_GEMINI_HELPER_DELAY"); delay != "" {
		duration, err := time.ParseDuration(delay)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(95)
		}
		time.Sleep(duration)
	}
	if detail := os.Getenv("AIR_GEMINI_HELPER_FAILURE"); detail != "" {
		fmt.Fprintln(os.Stderr, detail)
		os.Exit(42)
	}
	fmt.Fprintln(os.Stdout, os.Getenv("AIR_GEMINI_HELPER_RESULT"))
	os.Exit(0)
}

func TestGeminiReviewerRunsReadOnlyCommitReview(t *testing.T) {
	repository, directory := newTestGitRepository(t)
	base := testCommitFile(t, directory, "state.txt", []byte("old\n"), "base")
	head := testCommitFile(t, directory, "state.txt", []byte("new\n"), "change state")
	metadata, err := repository.CommitMetadata(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	structured := ReviewOutput{
		NewFindings: []NewFinding{{
			Severity: "warning", Title: "state regression",
			Description: "The new state violates the caller contract.", File: stringPointer("state.txt"),
		}},
		ResolvedFindings: []ResolvedFinding{{ID: 7, Reason: "The old failure is removed."}},
		Summary:          "Inspected the state transition.",
	}
	command, invocation := newGeminiTestCommand(t, "gemini-test-model", structured, "")
	reviewer := &GeminiReviewer{
		Repository: repository, Binary: "/custom/gemini", Model: "gemini-test-model",
		Effort: "default", CommandContext: command,
	}
	result, err := reviewer.Review(context.Background(), ReviewInput{
		Commit: metadata,
		OpenFindings: []Finding{{
			ID: 7, IntroducedSHA: base, Severity: "warning", Title: "old failure",
			Description: "An earlier commit introduced a failure.",
		}},
	})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if invocation.Name != "/custom/gemini" {
		t.Fatalf("binary = %q", invocation.Name)
	}
	assertArgumentPair(t, invocation.Args, "--model", "gemini-test-model")
	assertArgumentPair(t, invocation.Args, "--prompt", "")
	assertArgumentPair(t, invocation.Args, "--output-format", "json")
	assertArgumentPair(t, invocation.Args, "--approval-mode", "plan")
	assertArgumentPair(t, invocation.Args, "--extensions", "none")
	assertArgumentPair(t, invocation.Args, "--allowed-mcp-server-names", "")
	if !slices.Contains(invocation.Args, "--skip-trust") {
		t.Fatalf("args lack --skip-trust: %q", invocation.Args)
	}
	prompt, err := os.ReadFile(invocation.PromptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prompt), head) || !strings.Contains(string(prompt), "old failure") ||
		!strings.Contains(string(prompt), `<output_schema>`) ||
		!strings.Contains(string(prompt), `"new_findings"`) {
		t.Fatalf("prompt lacks review context:\n%s", prompt)
	}
	policy, err := os.ReadFile(invocation.PolicyPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`toolName = "*"`, `mcpName = "*"`, `"git show"`, `"git diff"`} {
		if !strings.Contains(string(policy), required) {
			t.Fatalf("policy lacks %q:\n%s", required, policy)
		}
	}
	settings := readGeminiTestSettings(t, invocation.SettingsPath)
	if settingAt(t, settings, "hooksConfig", "enabled") != false ||
		settingAt(t, settings, "skills", "enabled") != false ||
		settingAt(t, settings, "general", "plan", "modelRouting") != false ||
		settingAt(t, settings, "admin", "secureModeEnabled") != true ||
		settingAt(t, settings, "admin", "extensions", "enabled") != false ||
		settingAt(t, settings, "admin", "mcp", "enabled") != false ||
		settingAt(t, settings, "admin", "skills", "enabled") != false ||
		settingAt(t, settings, "security", "toolSandboxing") != true ||
		settingAt(t, settings, "security", "disableYoloMode") != true ||
		settingAt(t, settings, "advanced", "ignoreLocalEnv") != true {
		t.Fatalf("unsafe Gemini settings: %#v", settings)
	}
	core, ok := settingAt(t, settings, "tools", "core").([]any)
	if !ok {
		t.Fatalf("tools.core = %#v", settingAt(t, settings, "tools", "core"))
	}
	coreJSON, _ := json.Marshal(core)
	if !strings.Contains(string(coreJSON), `"read_file"`) ||
		!strings.Contains(string(coreJSON), `"run_shell_command(git show)"`) ||
		strings.Contains(string(coreJSON), `"write_file"`) {
		t.Fatalf("tools.core = %s", coreJSON)
	}
	if len(result.Output.NewFindings) != 1 || len(result.Output.ResolvedFindings) != 1 {
		t.Fatalf("result = %+v", result.Output)
	}
	if result.Usage == nil || result.Usage.InputTokens != 105 || result.Usage.CachedInputTokens != 10 ||
		result.Usage.CacheWriteTokens != nil || result.Usage.OutputTokens != 25 ||
		result.Usage.ReasoningOutputTokens != 5 || result.Usage.ReasoningOutputTokensUnreported {
		t.Fatalf("usage = %+v", result.Usage)
	}
	if !strings.Contains(result.RawResponse, `"session_id":"test"`) {
		t.Fatalf("raw response = %q", result.RawResponse)
	}
}

func TestGeminiReviewerRechecksExactHEAD(t *testing.T) {
	repository, directory := newTestGitRepository(t)
	head := testCommitFile(t, directory, "state.go", []byte("package state\n"), "base")
	structured := RecheckOutput{Findings: []RecheckFindingResult{{
		ID: 7, Outcome: "still_present", Reason: "The invalid state remains reachable.",
	}}, Summary: "Checked the current implementation."}
	command, invocation := newGeminiTestCommand(t, "gemini-test-model", structured, "")
	var envelope struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal([]byte(invocation.Result), &envelope); err != nil {
		t.Fatal(err)
	}
	fenced, err := json.Marshal("```json\n" + envelope.Response + "\n```")
	if err != nil {
		t.Fatal(err)
	}
	invocation.Result = strings.Replace(invocation.Result,
		fmt.Sprintf("%q", envelope.Response), string(fenced), 1)
	reviewer := &GeminiReviewer{
		Repository: repository, Model: "gemini-test-model", Effort: "default",
		CommandContext: command,
	}
	result, err := reviewer.Recheck(context.Background(), RecheckInput{
		HeadSHA: head,
		Findings: []Finding{{
			ID: 7, IntroducedSHA: head, Severity: "warning", Title: "invalid state",
			Description: "The state can become invalid.", File: stringPointer("state.go"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Output.Findings) != 1 || result.Output.Findings[0].Outcome != "still_present" {
		t.Fatalf("recheck result = %+v", result)
	}
	prompt, err := os.ReadFile(invocation.PromptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prompt), head) || !strings.Contains(string(prompt), "exact Git HEAD snapshot") {
		t.Fatalf("recheck prompt:\n%s", prompt)
	}
}

func TestGeminiReviewerReportsCommandFailureAndTimeout(t *testing.T) {
	repository, directory := newTestGitRepository(t)
	testCommitFile(t, directory, "state.txt", []byte("old\n"), "base")
	head := testCommitFile(t, directory, "state.txt", []byte("new\n"), "change")
	metadata, err := repository.CommitMetadata(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("failure", func(t *testing.T) {
		command, _ := newGeminiTestCommand(t, "gemini-test-model", ReviewOutput{}, "authentication required")
		reviewer := &GeminiReviewer{
			Repository: repository, Model: "gemini-test-model", Effort: "default", CommandContext: command,
		}
		_, err := reviewer.Review(context.Background(), ReviewInput{Commit: metadata})
		if err == nil || !strings.Contains(err.Error(), "Gemini failed: authentication required") {
			t.Fatalf("Review error = %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		command, invocation := newGeminiTestCommand(t, "gemini-test-model", ReviewOutput{}, "")
		invocation.Delay = "5s"
		reviewer := &GeminiReviewer{
			Repository: repository, Model: "gemini-test-model", Effort: "default",
			Timeout: 100 * time.Millisecond, CommandContext: command,
		}
		_, err := reviewer.Review(context.Background(), ReviewInput{Commit: metadata})
		if err == nil || !strings.Contains(err.Error(), "Gemini timed out after 100ms") {
			t.Fatalf("Review error = %v", err)
		}
	})
}

func TestGeminiReviewerRequiresDefaultEffort(t *testing.T) {
	repository, directory := newTestGitRepository(t)
	testCommitFile(t, directory, "state.txt", []byte("old\n"), "base")
	head := testCommitFile(t, directory, "state.txt", []byte("new\n"), "change")
	metadata, err := repository.CommitMetadata(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	reviewer := &GeminiReviewer{Repository: repository, Model: "gemini-test-model", Effort: "high"}
	_, err = reviewer.Review(context.Background(), ReviewInput{Commit: metadata})
	if err == nil || !strings.Contains(err.Error(), `use effort "default"`) {
		t.Fatalf("Review error = %v", err)
	}
}

func TestParseGeminiResultValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		json string
		want string
	}{
		{"response", `{"stats":{"models":{"gemini-test":{"tokens":{}}}}}`, "omitted response"},
		{"stats", `{"response":"{}"}`, "omitted per-model token usage"},
		{"model", `{"response":"{}","stats":{"models":{"other":{"tokens":{}}}}}`, `reported model "other"`},
		{"multiple models", `{"response":"{}","stats":{"models":{"gemini-test":{"tokens":{}},"other":{"tokens":{}}}}}`, "requires exactly one"},
		{"tokens", `{"response":"{}","stats":{"models":{"gemini-test":{}}}}`, "omitted model token counts"},
		{"prompt", `{"response":"{}","stats":{"models":{"gemini-test":{"tokens":{"input":1,"prompt":2}}}}}`, "input plus cached is 1"},
		{"total", `{"response":"{}","stats":{"models":{"gemini-test":{"tokens":{"input":2,"prompt":2,"candidates":3,"total":9}}}}}`, "categorized total is 5"},
		{"error", `{"error":{"type":"AuthError","message":"sign in first"}}`, "Gemini failed: sign in first"},
		{"structured JSON", `{"response":"not JSON","stats":{"models":{"gemini-test":{"tokens":{}}}}}`, "response is not valid JSON"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := parseGeminiResult([]byte(test.json), "gemini-test")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestGeminiSystemSettingsPreserveExistingRestrictions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "system-settings.json")
	if err := os.WriteFile(path, []byte(`{
		"model":{"name":"gemini-fixed"},
		"admin":{"customRestriction":"preserved"},
		"hooksConfig":{"enabled":true}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reviewer := &GeminiReviewer{SystemSettingsPath: path}
	contents, err := reviewer.geminiSystemSettings()
	if err != nil {
		t.Fatal(err)
	}
	settings := make(map[string]any)
	if err := json.Unmarshal(contents, &settings); err != nil {
		t.Fatal(err)
	}
	if settingAt(t, settings, "model", "name") != "gemini-fixed" ||
		settingAt(t, settings, "admin", "customRestriction") != "preserved" ||
		settingAt(t, settings, "hooksConfig", "enabled") != false {
		t.Fatalf("merged settings = %#v", settings)
	}
}

func readGeminiTestSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	settings := make(map[string]any)
	if err := json.Unmarshal(contents, &settings); err != nil {
		t.Fatal(err)
	}
	return settings
}

func settingAt(t *testing.T, settings map[string]any, path ...string) any {
	t.Helper()
	var value any = settings
	for _, key := range path {
		object, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("setting parent %q = %#v", key, value)
		}
		value, ok = object[key]
		if !ok {
			t.Fatalf("setting %q is absent", strings.Join(path, "."))
		}
	}
	return value
}
