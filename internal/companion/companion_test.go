package companion

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestApproverHelper(t *testing.T) {
	if os.Getenv("PLY_TEST_APPROVER") == "" {
		return
	}
	args := []string{}
	for n, a := range os.Args {
		if a == "--" {
			args = os.Args[n+1:]
			break
		}
	}
	os.Exit(Approve(os.Getenv("PLY_TEST_APPROVER"), args))
}
func approver(t *testing.T, kind string, env map[string]string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestApproverHelper$", "--"}, args...)...)
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, "PLY_TEST_APPROVER="+kind)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	b, e := cmd.CombinedOutput()
	return exitCode(e), string(b)
}
func TestApproverContracts(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "allowlist"), []byte("# first matching regex wins\ndeny ^rm\\b\nallow ^pwd$\n"), 0600)
	for _, tc := range []struct {
		kind, command string
		code          int
	}{{"yolo", "anything", 0}, {"allowlist", "pwd", 0}, {"allowlist", "rm file", 1}, {"allowlist", "echo hello", 2}} {
		code, out := approver(t, tc.kind, map[string]string{"PLY_CONFIG_DIR": dir, "PLY_COMMAND": tc.command})
		if code != tc.code {
			t.Fatal(tc, code, out)
		}
	}
	for _, kind := range []string{"ask", "auto", "allowlist", "chain", "yolo"} {
		code, out := approver(t, kind, nil, "--ply-prompt")
		if code != 0 || out != "" {
			t.Fatal(kind, code, out)
		}
	}
}
func TestChainAndAuto(t *testing.T) {
	dir := t.TempDir()
	scripts := map[string]string{"abstain": "exit 2", "deny": "echo denied; exit 1", "allow": "exit 0", "ply": "printf '%s\\n' \"$PLY_TEST_DECISION\""}
	for name, body := range scripts {
		os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0700)
	}
	code, out := approver(t, "chain", nil, filepath.Join(dir, "abstain"), filepath.Join(dir, "deny"), filepath.Join(dir, "allow"))
	if code != 1 || !strings.Contains(out, "denied") {
		t.Fatal(code, out)
	}
	code, out = approver(t, "chain", nil, filepath.Join(dir, "abstain"), filepath.Join(dir, "allow"))
	if code != 0 {
		t.Fatal(code, out)
	}
	for _, tc := range []struct {
		decision string
		code     int
	}{{"APPROVE", 0}, {"DENY", 1}, {"UNSURE", 2}, {"APPROVE with more text", 2}} {
		code, out := approver(t, "auto", map[string]string{"PATH": dir + ":" + os.Getenv("PATH"), "PLY_TEST_DECISION": tc.decision})
		if code != tc.code {
			t.Fatal(tc, code, out)
		}
	}
}
func TestMCPStdioHelper(t *testing.T) {
	if os.Getenv("PLY_TEST_MCP") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req rpc
		json.Unmarshal(scanner.Bytes(), &req)
		if req["id"] == nil {
			continue
		}
		var result rpc
		switch req["method"] {
		case "initialize":
			result = rpc{"protocolVersion": "2025-11-25", "capabilities": rpc{}, "serverInfo": rpc{"name": "mock", "version": "1"}}
		case "tools/list":
			result = rpc{"tools": []rpc{{"name": "echo", "inputSchema": rpc{"type": "object"}}}}
		case "tools/call":
			result = rpc{"content": []rpc{{"type": "text", "text": "hello"}}}
		}
		json.NewEncoder(os.Stdout).Encode(rpc{"jsonrpc": "2.0", "id": req["id"], "result": result})
	}
	os.Exit(0)
}
func TestMCPStdio(t *testing.T) {
	c, close, e := connectMCP(map[string]server{"test": {Command: os.Args[0], Args: []string{"-test.run=^TestMCPStdioHelper$"}, Env: map[string]string{"PLY_TEST_MCP": "1"}}}, "test")
	if e != nil {
		t.Fatal(e)
	}
	defer close()
	tools, e := c.tools()
	if e != nil || len(tools) != 1 || tools[0]["name"] != "echo" {
		t.Fatal(tools, e)
	}
	r, e := c.request("tools/call", rpc{"name": "echo", "arguments": rpc{}})
	if e != nil || r["content"] == nil {
		t.Fatal(r, e)
	}
}
func TestMCPHTTPAndSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(204)
			return
		}
		var req rpc
		json.NewDecoder(r.Body).Decode(&req)
		if req["method"] == "initialize" {
			w.Header().Set("MCP-Session-Id", "test-session")
			json.NewEncoder(w).Encode(rpc{"jsonrpc": "2.0", "id": req["id"], "result": rpc{"protocolVersion": "2025-11-25"}})
			return
		}
		if r.Header.Get("MCP-Session-Id") != "test-session" || r.Header.Get("MCP-Protocol-Version") != "2025-11-25" {
			t.Error("missing protocol headers")
		}
		if req["id"] == nil {
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(rpc{"jsonrpc": "2.0", "id": req["id"], "result": rpc{"tools": []rpc{{"name": "remote"}}}})
		fmt.Fprintf(w, "data: %s\n\n", b)
	}))
	defer srv.Close()
	c, close, e := connectMCP(map[string]server{"test": {URL: srv.URL}}, "test")
	if e != nil {
		t.Fatal(e)
	}
	defer close()
	tools, e := c.tools()
	if e != nil || len(tools) != 1 || tools[0]["name"] != "remote" {
		t.Fatal(tools, e)
	}
}
func TestMCPRejectsWrongIDAndErrors(t *testing.T) {
	for _, r := range []rpc{{"id": 2, "result": rpc{}}, {"id": 1, "error": rpc{"message": "failed"}}, {"id": 1, "result": nil}} {
		if _, e := rpcResult(r, 1); e == nil {
			t.Fatal(r)
		}
	}
}
func TestSkillDiscovery(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := filepath.Join(dir, "ply", "skills", "sample")
	os.MkdirAll(path, 0700)
	os.WriteFile(filepath.Join(path, "SKILL.md"), []byte("---\nname: sample\ndescription: A sample skill\n---\nDo the work.\n"), 0600)
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code := Skill([]string{"show", "sample"})
	w.Close()
	os.Stdout = old
	var b bytes.Buffer
	b.ReadFrom(r)
	r.Close()
	if code != 0 || !strings.Contains(b.String(), "Do the work.") {
		t.Fatal(code, b.String())
	}
}
func TestMCPRequestHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &mcpClient{ctx: ctx, s: server{URL: "http://127.0.0.1:1"}}
	if _, e := c.request("tools/list", rpc{}); e == nil {
		t.Fatal("ignored canceled context")
	}
}

func TestSkillPrecedence(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "project")
	nested := filepath.Join(project, "nested")
	os.MkdirAll(nested, 0700)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Chdir(nested)
	write := func(root, name, text string) {
		path := filepath.Join(root, "skills", name)
		os.MkdirAll(path, 0700)
		os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(text), 0600)
	}
	write(filepath.Join(project, ".ply"), "same", "parent ply")
	write(filepath.Join(project, ".agents"), "same", "parent agents")
	write(filepath.Join(nested, ".agents"), "same", "near agents")
	write(filepath.Join(nested, ".ply"), "same", "near ply")
	write(filepath.Join(home, ".agents"), "user", "user agents")
	write(UserDir(), "user", "user ply")
	write(filepath.Join(home, ".agents"), "agents-only", "shared skill")
	read := func(name string) string {
		f, e := os.CreateTemp(t.TempDir(), "stdout")
		if e != nil {
			t.Fatal(e)
		}
		defer f.Close()
		old := os.Stdout
		os.Stdout = f
		code := Skill([]string{"show", name})
		os.Stdout = old
		b, _ := os.ReadFile(f.Name())
		if code != 0 {
			t.Fatal(code)
		}
		return string(b)
	}
	if got := read("same"); got != "near ply" {
		t.Fatal(got)
	}
	os.RemoveAll(filepath.Join(nested, ".ply"))
	if got := read("same"); got != "near agents" {
		t.Fatal(got)
	}
	if got := read("user"); got != "user ply" {
		t.Fatal(got)
	}
	if got := read("agents-only"); got != "shared skill" {
		t.Fatal(got)
	}
}
