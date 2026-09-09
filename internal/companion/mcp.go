package companion

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

type server struct {
	Command string            `toml:"command"`
	Args    []string          `toml:"args"`
	Env     map[string]string `toml:"env"`
	URL     string            `toml:"url"`
	Headers map[string]string `toml:"headers"`
}
type rpc map[string]any
type mcpClient struct {
	ctx              context.Context
	s                server
	cmd              *exec.Cmd
	in               io.WriteCloser
	reader           *bufio.Reader
	session, version string
	id               int
}

func mcpServers() (map[string]server, error) {
	result := map[string]server{}
	dirs := projectDirs()
	paths := []string{filepath.Join(UserDir(), "mcp.toml")}
	for n := len(dirs) - 1; n >= 0; n-- {
		paths = append(paths, filepath.Join(dirs[n], "mcp.toml"))
	}
	for _, path := range paths {
		b, e := os.ReadFile(path)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return nil, e
		}
		var conf struct {
			Servers map[string]server `toml:"servers"`
		}
		if e = toml.Unmarshal(b, &conf); e != nil {
			return nil, e
		}
		for k, v := range conf.Servers {
			result[k] = v
		}
	}
	return result, nil
}
func MCP(args []string) int {
	if e := mcpMain(args); e != nil {
		fmt.Fprintln(os.Stderr, "ply-mcp:", e)
		return 1
	}
	return 0
}
func mcpMain(args []string) error {
	servers, e := mcpServers()
	if e != nil {
		return e
	}
	names := []string{}
	for k := range servers {
		names = append(names, k)
	}
	sort.Strings(names)
	if len(args) == 1 && args[0] == "--ply-prompt" {
		if len(names) > 0 {
			fmt.Printf("MCP tools: use ply-mcp list, ply-mcp schema SERVER TOOL, ply-mcp call SERVER TOOL 'JSON'. Servers: %s\n", strings.Join(names, ", "))
		}
		return nil
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: ply-mcp list [SERVER] | schema SERVER TOOL | call SERVER TOOL JSON")
	}
	if args[0] == "list" {
		if len(args) > 2 {
			return fmt.Errorf("list accepts at most one server")
		}
		if len(args) == 2 {
			names = []string{args[1]}
		}
		for _, name := range names {
			c, close, e := connectMCP(servers, name)
			if e != nil {
				return e
			}
			tools, e := c.tools()
			close()
			if e != nil {
				return e
			}
			for _, tool := range tools {
				b, _ := json.Marshal(rpc{"server": name, "tool": tool})
				fmt.Println(string(b))
			}
		}
		return nil
	}
	if len(args) < 3 {
		return fmt.Errorf("server and tool required")
	}
	c, close, e := connectMCP(servers, args[1])
	if e != nil {
		return e
	}
	defer close()
	switch args[0] {
	case "schema":
		if len(args) != 3 {
			return fmt.Errorf("schema takes SERVER TOOL")
		}
		tools, e := c.tools()
		if e != nil {
			return e
		}
		for _, tool := range tools {
			if tool["name"] == args[2] {
				b, _ := json.MarshalIndent(tool, "", "  ")
				fmt.Println(string(b))
				return nil
			}
		}
		return fmt.Errorf("tool %s not found", args[2])
	case "call":
		if len(args) != 4 {
			return fmt.Errorf("call takes SERVER TOOL JSON")
		}
		var arguments map[string]any
		if e = json.Unmarshal([]byte(args[3]), &arguments); e != nil {
			return e
		}
		if arguments == nil {
			return fmt.Errorf("arguments must be a JSON object")
		}
		r, e := c.request("tools/call", rpc{"name": args[2], "arguments": arguments})
		if e != nil {
			return e
		}
		b, _ := json.MarshalIndent(r, "", "  ")
		fmt.Println(string(b))
		if failed, _ := r["isError"].(bool); failed {
			return fmt.Errorf("tool returned isError")
		}
		return nil
	}
	return fmt.Errorf("unknown command %s", args[0])
}
func connectMCP(servers map[string]server, name string) (*mcpClient, func(), error) {
	s, ok := servers[name]
	if !ok {
		return nil, nil, fmt.Errorf("unknown MCP server %s", name)
	}
	if (s.Command == "") == (s.URL == "") {
		return nil, nil, fmt.Errorf("server %s requires exactly one of command or url", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	c := &mcpClient{ctx: ctx, s: s, version: "2025-11-25"}
	close := func() {
		if c.session != "" {
			req, e := http.NewRequestWithContext(ctx, "DELETE", s.URL, nil)
			if e == nil {
				c.headers(req)
				resp, e := http.DefaultClient.Do(req)
				if e == nil {
					resp.Body.Close()
				}
			}
		}
		cancel()
		if c.in != nil {
			c.in.Close()
		}
		if c.cmd != nil {
			c.cmd.Wait()
		}
	}
	if s.Command != "" {
		c.cmd = exec.CommandContext(ctx, s.Command, s.Args...)
		c.cmd.Stderr = os.Stderr
		c.cmd.Env = os.Environ()
		for k, v := range s.Env {
			c.cmd.Env = append(c.cmd.Env, k+"="+os.ExpandEnv(v))
		}
		in, e := c.cmd.StdinPipe()
		if e != nil {
			cancel()
			return nil, nil, e
		}
		c.in = in
		out, e := c.cmd.StdoutPipe()
		if e != nil {
			close()
			return nil, nil, e
		}
		c.reader = bufio.NewReader(out)
		if e = c.cmd.Start(); e != nil {
			close()
			return nil, nil, e
		}
	}
	r, e := c.request("initialize", rpc{"protocolVersion": c.version, "capabilities": rpc{}, "clientInfo": rpc{"name": "ply-mcp", "version": "0.1.0"}})
	if e != nil {
		close()
		return nil, nil, e
	}
	version, _ := r["protocolVersion"].(string)
	switch version {
	case "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05":
		c.version = version
	default:
		close()
		return nil, nil, fmt.Errorf("unsupported MCP protocol version %q", version)
	}
	if e = c.send(rpc{"jsonrpc": "2.0", "method": "notifications/initialized"}); e != nil {
		close()
		return nil, nil, e
	}
	return c, close, nil
}
func (c *mcpClient) headers(r *http.Request) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", c.version)
	if c.session != "" {
		r.Header.Set("MCP-Session-Id", c.session)
	}
	for k, v := range c.s.Headers {
		r.Header.Set(k, os.ExpandEnv(v))
	}
}
func (c *mcpClient) send(m rpc) error { _, e := c.exchange(m, false); return e }
func (c *mcpClient) request(method string, params rpc) (rpc, error) {
	c.id++
	return c.exchange(rpc{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params}, true)
}
func (c *mcpClient) exchange(m rpc, reply bool) (rpc, error) {
	b, e := json.Marshal(m)
	if e != nil {
		return nil, e
	}
	reader := c.reader
	var body io.ReadCloser
	sse := false
	if c.in != nil {
		if _, e = c.in.Write(append(b, '\n')); e != nil {
			return nil, e
		}
		if !reply {
			return nil, nil
		}
	} else {
		req, e := http.NewRequestWithContext(c.ctx, "POST", c.s.URL, bytes.NewReader(b))
		if e != nil {
			return nil, e
		}
		c.headers(req)
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			return nil, e
		}
		body = resp.Body
		defer body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(body, 8192))
			return nil, fmt.Errorf("MCP HTTP %d: %s", resp.StatusCode, b)
		}
		if s := resp.Header.Get("MCP-Session-Id"); s != "" {
			c.session = s
		}
		if !reply {
			return nil, nil
		}
		sse = strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
		reader = bufio.NewReader(body)
		if !sse {
			var r rpc
			if e = json.NewDecoder(reader).Decode(&r); e != nil {
				return nil, e
			}
			return rpcResult(r, m["id"])
		}
	}
	var data strings.Builder
	for {
		line, e := reader.ReadString('\n')
		if e != nil && len(line) == 0 {
			return nil, e
		}
		line = strings.TrimRight(line, "\r\n")
		if sse {
			if strings.HasPrefix(line, "data:") {
				data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
				data.WriteByte('\n')
			}
			if line != "" {
				continue
			}
			line = data.String()
			data.Reset()
			if strings.TrimSpace(line) == "" {
				continue
			}
		}
		var r rpc
		if e = json.Unmarshal([]byte(line), &r); e != nil {
			return nil, e
		}
		if fmt.Sprint(r["id"]) == fmt.Sprint(m["id"]) && r["method"] == nil {
			return rpcResult(r, m["id"])
		}
		if r["method"] != nil && r["id"] != nil {
			answer := rpc{"jsonrpc": "2.0", "id": r["id"], "error": rpc{"code": -32601, "message": "Client capability not supported"}}
			if r["method"] == "ping" {
				delete(answer, "error")
				answer["result"] = rpc{}
			}
			if e = c.send(answer); e != nil {
				return nil, e
			}
		}
	}
}
func rpcResult(r rpc, id any) (rpc, error) {
	if fmt.Sprint(r["id"]) != fmt.Sprint(id) {
		return nil, fmt.Errorf("MCP response ID mismatch")
	}
	if r["error"] != nil {
		return nil, fmt.Errorf("MCP error: %v", r["error"])
	}
	result, ok := r["result"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid MCP result")
	}
	return result, nil
}
func (c *mcpClient) tools() ([]rpc, error) {
	var tools []rpc
	params := rpc{}
	seen := map[string]bool{}
	for {
		r, e := c.request("tools/list", params)
		if e != nil {
			return nil, e
		}
		list, _ := r["tools"].([]any)
		for _, v := range list {
			if m, ok := v.(map[string]any); ok {
				tools = append(tools, m)
			}
		}
		cursor, _ := r["nextCursor"].(string)
		if cursor == "" {
			return tools, nil
		}
		if seen[cursor] {
			return nil, fmt.Errorf("MCP repeated pagination cursor")
		}
		seen[cursor] = true
		params["cursor"] = cursor
	}
}
