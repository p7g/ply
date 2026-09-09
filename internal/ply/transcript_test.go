package ply

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, path string, items ...Item) {
	t.Helper()
	var b bytes.Buffer
	all := append([]Item{{"type": "ply.meta", "version": 1, "cwd": filepath.Dir(path)}}, items...)
	for n, i := range all {
		i["seq"] = n
		i["ts"] = "2026-09-08T00:00:00Z"
		if e := json.NewEncoder(&b).Encode(i); e != nil {
			t.Fatal(e)
		}
	}
	if e := os.WriteFile(path, b.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
}
func TestReplayPreservesOpaqueItemsAndPlan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	fixture(t, path, Item{"type": "ply.system", "text": "system"}, Item{"type": "ply.plan", "text": "exact plan\n  spaces"}, message("user", "old"), Item{"type": "ply.compaction", "summary": "summary"}, Item{"type": "reasoning", "id": "r", "encrypted_content": "opaque", "summary": []any{}, "ply.partial": true}, Item{"type": "ply.task_done", "task": "7", "exit_code": 0, "duration_s": 2, "output_tail": "ok"}, Item{"type": "ply.interrupt"}, message("user", "new"))
	items, e := readItems(path, false)
	if e != nil {
		t.Fatal(e)
	}
	r := Replay(items)
	if len(r) != 7 {
		t.Fatalf("replay: %#v", r)
	}
	if textOf(r[1]) != "Current plan:\nexact plan\n  spaces" || textOf(r[2]) != "summary" {
		t.Fatal(r)
	}
	if r[3]["encrypted_content"] != "opaque" || r[3]["seq"] != nil || r[3]["ply.partial"] != nil {
		t.Fatal(r[3])
	}
	if !strings.Contains(textOf(r[4]), "task 7 finished") {
		t.Fatal(r[4])
	}
	if textOf(r[6]) != "new" {
		t.Fatal(r[6])
	}
}
func TestAppendLockAndIncompleteTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	tr, e := openTranscript(path, dir)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = openTranscript(path, dir); e == nil {
		t.Fatal("second writer acquired lock")
	}
	if e = tr.Append(message("user", strings.Repeat("x", 200000))); e != nil {
		t.Fatal(e)
	}
	tr.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString(`{"seq":2`)
	f.Close()
	if _, e = readItems(path, false); e == nil {
		t.Fatal("accepted torn item")
	}
	items, e := readItems(path, true)
	if e != nil || len(items) != 2 {
		t.Fatalf("follow: %d %v", len(items), e)
	}
}
func TestRendererGolden(t *testing.T) {
	items := []Item{message("user", "hello\nworld"), message("assistant", "Looking."), {"type": "function_call", "name": "bash", "arguments": `{"command":"echo hi","timeout_s":5}`}, {"type": "function_call_output", "output": "hi\n[exit 0, 0.01s]"}, {"type": "ply.plan", "text": "plan"}, {"type": "ply.compaction", "summary": nil}}
	var b bytes.Buffer
	r := Renderer{W: &b}
	for _, i := range items {
		r.Item(i)
	}
	want, err := os.ReadFile("../../testdata/render.golden")
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != string(want) {
		t.Fatalf("render mismatch:\n%s", b.String())
	}
	b.Reset()
	r.Quiet = true
	for _, i := range items {
		r.Item(i)
	}
	if b.String() != "Looking.\n\n" {
		t.Fatal(b.String())
	}
}
func TestConfigPrecedenceAndTrust(t *testing.T) {
	user := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", user)
	os.MkdirAll(filepath.Join(user, "ply"), 0700)
	os.WriteFile(filepath.Join(user, "ply", "config.toml"), []byte("model = 'user-model'\ncontext_window = 1000\n[output]\nmax_lines=20\n"), 0600)
	os.Mkdir(filepath.Join(cwd, ".ply"), 0700)
	os.WriteFile(filepath.Join(cwd, ".ply", "config.toml"), []byte("model='hostile-model'\n[approve]\ncommand='bad'\n[output]\nmax_lines=30\n"), 0600)
	t.Setenv("PLY_OUTPUT_MAX_LINES", "40")
	o := Options{Overrides: map[string]string{"output.max_lines": "50", "pager": "false"}}
	c, e := resolve(cwd, o)
	if e != nil {
		t.Fatal(e)
	}
	if c.S("model") != "user-model" || c.N("output.max_lines") != 50 || c.B("pager") {
		t.Fatal(c)
	}
	o.Trust = true
	c, e = resolve(cwd, o)
	if e != nil || c.S("model") != "hostile-model" {
		t.Fatal(c, e)
	}
}
func TestOptions(t *testing.T) {
	o, e := Parse([]string{"-m", "one", "t.jsonl", "-m", "two", "--no-detach", "--no-show-thinking", "--compact-at=0.7"})
	if e != nil || len(o.Messages) != 2 || o.Overrides["detach"] != "false" || o.Overrides["compact_at"] != "0.7" {
		t.Fatal(o, e)
	}
	for _, args := range [][]string{{"--follow", "-m", "x", "t"}, {"--tail", "-1", "t"}, {"--clear", "--compact", "t"}, {"--unknown", "t"}} {
		if _, e := Parse(args); e == nil {
			t.Fatal(args)
		}
	}
}

func TestKillOptionalIDAndBooleanInverses(t *testing.T) {
	for _, tc := range []struct {
		args []string
		id   string
	}{{[]string{"--kill", "t.jsonl", "--quiet"}, "all"}, {[]string{"--kill", "123", "--quiet", "t.jsonl"}, "123"}, {[]string{"t.jsonl", "--kill", "123"}, "123"}, {[]string{"--kill=123", "t.jsonl"}, "123"}} {
		o, e := Parse(tc.args)
		if e != nil || o.Kill != tc.id || o.Path != "t.jsonl" {
			t.Fatal(tc, o, e)
		}
	}
	o, e := Parse([]string{"--no-detach=false", "t.jsonl"})
	if e != nil || o.Overrides["detach"] != "true" {
		t.Fatal(o, e)
	}
}
