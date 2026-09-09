package ply

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResponsePersistenceAndApprovalOrder(t *testing.T) {
	dir := t.TempDir()
	policy := filepath.Join(dir, "approve")
	// All calls and response usage must already be durable when approval starts.
	body := "#!/bin/sh\n[ \"$(grep -c '\"type\":\"function_call\"' \"$PLY_TRANSCRIPT\")\" = 3 ] || exit 1\ngrep -q '\"type\":\"ply.usage\"' \"$PLY_TRANSCRIPT\" || exit 1\necho APPROVAL >&2\n"
	if e := os.WriteFile(policy, []byte(body), 0700); e != nil {
		t.Fatal(e)
	}
	record := recorded(t, dir, []Item{
		Item{"type": "reasoning", "id": "reason", "encrypted_content": "opaque", "summary": []any{Item{"type": "summary_text", "text": "Inspect first."}}},
		callItem("bash", "first", BashArgs{Command: "printf FIRST_RESULT", Justification: "inspect"}),
		callItem("plan", "plan", Item{"text": "The saved plan"}),
		callItem("bash", "second", BashArgs{Command: "printf SECOND_RESULT", Justification: "inspect"}),
	}, []Item{message("assistant", "Finished.")})
	out, e := cli(t, dir, "--plan", "--show-thinking", "--approve-command", policy, "--provider", "replay:"+record, "-m", "plan", "t.jsonl")
	if e != nil {
		t.Fatal(e, out)
	}
	cursor := 0
	for _, want := range []string{"$ printf FIRST_RESULT", "APPROVAL", "  FIRST_RESULT", "[plan updated]", "$ printf SECOND_RESULT", "APPROVAL", "  SECOND_RESULT", "Finished.", "The saved plan"} {
		n := strings.Index(out[cursor:], want)
		if n < 0 {
			t.Fatalf("missing ordered %q in %s", want, out)
		}
		cursor += n + len(want)
	}
	if strings.Count(out, "[plan updated]") != 1 || strings.Contains(out, "Plan updated.") {
		t.Fatal(out)
	}
	items := mustItems(t, filepath.Join(dir, "t.jsonl"))
	approvals, usage := 0, 0
	for _, i := range items {
		if str(i["type"]) == "ply.approval" {
			approvals++
			ref := num(i["for"])
			if str(items[ref]["type"]) != "function_call" {
				t.Fatal(i)
			}
		}
		if str(i["type"]) == "ply.usage" {
			usage++
		}
	}
	if approvals != 2 || usage != 2 || len(pending(items)) != 0 || latest(items, "reasoning") == nil {
		t.Fatal(items)
	}
}

func TestCredentialConfigAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "user"))
	t.Setenv("PLY_API_KEY", "environment-secret")
	if e := os.MkdirAll(filepath.Join(dir, ".ply"), 0700); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(dir, ".ply", "config.toml"), []byte("api_key = 'project-secret'\n"), 0600)
	c, e := resolve(dir, Options{})
	if e != nil || c.S("api_key") != "environment-secret" {
		t.Fatal(c, e)
	}
	c, e = resolve(dir, Options{Overrides: map[string]string{"api_key": "flag-secret"}})
	if e != nil || c.S("api_key") != "flag-secret" {
		t.Fatal(c, e)
	}
	b, _ := json.Marshal(c.snapshot())
	if strings.Contains(string(b), "secret") || c.snapshot()["api_key"] != nil {
		t.Fatal(string(b))
	}
	t.Setenv("PLY_API_KEY", "")
	os.Unsetenv("PLY_API_KEY")
	c, e = resolve(dir, Options{})
	if e != nil || c.S("api_key") != "" {
		t.Fatal(c, e)
	}
	c, e = resolve(dir, Options{Trust: true})
	if e != nil || c.S("api_key") != "project-secret" {
		t.Fatal(c, e)
	}
	record := recorded(t, dir, []Item{message("assistant", "ok")})
	out, e := cli(t, dir, "--api-key", "flag-secret", "--provider", "replay:"+record, "-m", "hello", "t.jsonl")
	if e != nil {
		t.Fatal(e, out)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "t.jsonl"))
	if strings.Contains(string(raw), "flag-secret") {
		t.Fatal("credential recorded")
	}
	out, e = cli(t, dir, "--api-key", "flag-secret", "--show-config", "t.jsonl")
	if e != nil || strings.Contains(out, "flag-secret") || !strings.Contains(out, "[redacted]") {
		t.Fatal(e, out)
	}
}

func TestModesAreTurnScopedAndSurviveCompaction(t *testing.T) {
	mode := message("developer", "planning only")
	mode["ply.mode"] = "plan"
	items := []Item{mode, message("user", "old"), Item{"type": "ply.compaction", "summary": "summary"}, message("user", "new")}
	a := App{T: &Transcript{Items: items}, Modes: []Item{mode}}
	input := a.modelInput()
	count := 0
	for _, i := range input {
		if textOf(i) == "planning only" {
			count++
			if i["ply.mode"] != nil {
				t.Fatal(i)
			}
		}
	}
	if count != 1 {
		t.Fatal(input)
	}
	a.Modes = nil
	for _, i := range a.modelInput() {
		if textOf(i) == "planning only" {
			t.Fatal("old mode leaked")
		}
	}
}

func TestApprovalEffectiveSettingsAndPlanPropagation(t *testing.T) {
	dir := t.TempDir()
	policy := filepath.Join(dir, "approve")
	os.WriteFile(policy, []byte("#!/bin/sh\n[ \"$PLY_PLAN_MODE\" = 1 ] && [ \"$PLY_MODEL\" = main ] && [ \"$PLY_APPROVE_MODEL\" = reviewer ] && [ \"$PLY_API_KEY\" = private ] && [ \"$PLY_APPROVE_CONTEXT_WINDOW\" = 4000 ]\n"), 0700)
	record := recorded(t, dir, []Item{callItem("bash", "read", BashArgs{Command: "pwd", Justification: "inspect"})}, []Item{message("assistant", "done")})
	out, e := cli(t, dir, "--plan", "--model", "main", "--approve-model", "reviewer", "--approve-context-window", "4000", "--api-key", "private", "--approve-command", policy, "--provider", "replay:"+record, "-m", "inspect", "t.jsonl")
	if e != nil || latest(mustItems(t, filepath.Join(dir, "t.jsonl")), "ply.approval")["result"] != "approved" {
		t.Fatal(e, out)
	}
}

func TestQuietUsageAndStatus(t *testing.T) {
	dir := t.TempDir()
	record := recorded(t, dir, []Item{message("assistant", "Only prose.")})
	out, e := cli(t, dir, "-q", "--provider", "replay:"+record, "-m", "hello", "t.jsonl")
	if e != nil || out != "Only prose.\n\n" {
		t.Fatal(e, out)
	}
}

func TestNestedPlanApproval(t *testing.T) {
	dir := t.TempDir()
	c, e := resolve(dir, Options{Overrides: map[string]string{"approve.command": "test \"$PLY_PLAN_MODE\" = 1"}})
	if e != nil {
		t.Fatal(e)
	}
	transcript, e := openTranscript(filepath.Join(dir, "t.jsonl"), dir)
	if e != nil {
		t.Fatal(e)
	}
	defer transcript.Close()
	a := App{T: transcript, Config: c, Context: context.Background(), Cwd: dir, Path: transcript.File.Name()}
	var reply bytes.Buffer
	if e = a.proxy(proxyRequest{Event: Item{"id": "child", "command": "pwd", "plan_mode": true}, Input: &reply, For: 0}); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(reply.String(), "\"ok\":true") || a.Options.Plan {
		t.Fatal(reply.String(), a.Options)
	}
}

func TestApprovalModelSubprocessEnvironment(t *testing.T) {
	dir := t.TempDir()
	record := recorded(t, dir, []Item{message("assistant", "ok")})
	cmd := exec.Command(filepath.Join(binaries, "ply"), "--provider", "replay:"+record, "--no-tools", "-q", "-m", "hello", "t.jsonl")
	cmd.Dir = dir
	cmd.Env = replaceEnv(integrationEnv(dir), map[string]string{"PLY_API_KEY": "env-secret"})
	out, e := cmd.CombinedOutput()
	if e != nil || string(out) != "ok\n\n" {
		t.Fatal(e, string(out))
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "t.jsonl"))
	if strings.Contains(string(raw), "env-secret") {
		t.Fatal("secret persisted")
	}
}

func TestApprovalInterruptDoesNotExecute(t *testing.T) {
	dir := t.TempDir()
	record := recorded(t, dir, []Item{callItem("bash", "blocked", BashArgs{Command: "touch executed", Justification: "test"})})
	cmd := exec.Command(filepath.Join(binaries, "ply"), "--approve-command", "touch approving; exec sleep 20", "--provider", "replay:"+record, "-m", "run", "t.jsonl")
	cmd.Dir = dir
	cmd.Env = integrationEnv(dir)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer cmd.Process.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, e := os.Stat(filepath.Join(dir, "approving")); e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("approval did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cmd.Process.Signal(os.Interrupt)
	e := cmd.Wait()
	if ee, ok := e.(*exec.ExitError); !ok || ee.ExitCode() != 130 {
		t.Fatal(e, out.String())
	}
	if strings.Contains(out.String(), "context canceled") || strings.Count(out.String(), "[interrupted]") != 1 {
		t.Fatal(out.String())
	}
	if _, e := os.Stat(filepath.Join(dir, "executed")); !os.IsNotExist(e) {
		t.Fatal("denied command executed")
	}
}
