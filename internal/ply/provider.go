package ply

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type Response struct {
	Output []Item `json:"output"`
	Usage  struct {
		Input   int `json:"input_tokens"`
		Output  int `json:"output_tokens"`
		Details struct {
			Cached int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
	Status string `json:"status"`
	Error  any    `json:"error"`
}
type Provider struct {
	Config   Config
	Recorded []Response
	Position int
	Client   *http.Client
	Replay   bool
}

func newProvider(c Config, s string) (*Provider, error) {
	p := &Provider{Config: c, Client: &http.Client{Timeout: 30 * time.Minute}}
	if s != "" {
		if !strings.HasPrefix(s, "replay:") {
			return nil, fmt.Errorf("unknown provider %s", s)
		}
		p.Replay = true
		items, e := readItems(strings.TrimPrefix(s, "replay:"), false)
		if e != nil {
			return nil, e
		}
		var r Response
		for _, i := range items {
			switch str(i["type"]) {
			case "message":
				if str(i["role"]) == "assistant" {
					r.Output = append(r.Output, clean(i))
				}
			case "reasoning", "function_call":
				r.Output = append(r.Output, clean(i))
			case "ply.compaction":
				if summary, ok := i["summary"].(string); ok && len(p.Recorded) > 0 {
					last := &p.Recorded[len(p.Recorded)-1]
					if len(last.Output) == 0 {
						last.Output = []Item{message("assistant", summary)}
					}
				}
			case "ply.usage":
				r.Usage.Input = num(i["input_tokens"])
				r.Usage.Output = num(i["output_tokens"])
				r.Usage.Details.Cached = num(i["cached_tokens"])
				p.Recorded = append(p.Recorded, r)
				r = Response{}
			}
		}
		if len(r.Output) > 0 {
			p.Recorded = append(p.Recorded, r)
		}
	}
	return p, nil
}

type providerError struct {
	message string
	retry   bool
}

func (e *providerError) Error() string { return e.message }
func (p *Provider) Call(ctx context.Context, input []Item, withTools bool, delta func(string)) (Response, string, error) {
	if ctx.Err() != nil {
		return Response{}, "", ctx.Err()
	}
	if p.Replay {
		if p.Position >= len(p.Recorded) {
			return Response{}, "", fmt.Errorf("replay provider exhausted")
		}
		r := p.Recorded[p.Position]
		p.Position++
		return r, "", nil
	}
	body := Item{"model": p.Config.S("model"), "input": input, "stream": true, "store": false, "include": []string{"reasoning.encrypted_content"}}
	if withTools {
		body["tools"] = toolSchemas
	}
	b, e := json.Marshal(body)
	if e != nil {
		return Response{}, "", e
	}
	for attempt := 0; ; attempt++ {
		r, partial, e := p.request(ctx, b, delta)
		if e == nil || ctx.Err() != nil {
			return r, partial, e
		}
		var pe *providerError
		if errors.As(e, &pe) && !pe.retry {
			return r, "", e
		}
		if attempt >= p.Config.N("provider.retries") {
			return r, "", e
		}
		if partial != "" && tty(os.Stderr) {
			fmt.Fprintln(os.Stderr, "[stream failed; retrying]")
		}
		delay := time.Second * time.Duration(1<<min(attempt, 6))
		select {
		case <-ctx.Done():
			return Response{}, "", ctx.Err()
		case <-time.After(delay):
		}
	}
}
func (p *Provider) request(ctx context.Context, b []byte, delta func(string)) (Response, string, error) {
	req, e := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(p.Config.S("base_url"), "/")+"/responses", bytes.NewReader(b))
	if e != nil {
		return Response{}, "", &providerError{e.Error(), false}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if key := p.Config.S("api_key"); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, e := p.Client.Do(req)
	if e != nil {
		return Response{}, "", e
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return Response{}, "", &providerError{fmt.Sprintf("provider HTTP %d: %s", resp.StatusCode, b), resp.StatusCode == 429 || resp.StatusCode >= 500}
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		var r Response
		decoder := json.NewDecoder(resp.Body)
		decoder.UseNumber()
		e = decoder.Decode(&r)
		if e == nil && (r.Error != nil || r.Status == "failed") {
			e = &providerError{fmt.Sprintf("provider failed: %v", r.Error), false}
		}
		return r, "", e
	}
	br := bufio.NewReader(resp.Body)
	var data, partial strings.Builder
	process := func() (Response, bool, error) {
		if data.Len() == 0 {
			return Response{}, false, nil
		}
		raw := strings.TrimSuffix(data.String(), "\n")
		data.Reset()
		if raw == "[DONE]" {
			return Response{}, false, nil
		}
		var event struct {
			Type     string   `json:"type"`
			Delta    string   `json:"delta"`
			Response Response `json:"response"`
			Message  string   `json:"message"`
		}
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		if e := decoder.Decode(&event); e != nil {
			return Response{}, false, e
		}
		switch event.Type {
		case "response.output_text.delta":
			partial.WriteString(event.Delta)
			if delta != nil {
				delta(event.Delta)
			}
		case "response.completed":
			return event.Response, true, nil
		case "response.failed", "response.incomplete", "error":
			return Response{}, false, &providerError{fmt.Sprintf("%s: %s %v", event.Type, event.Message, event.Response.Error), event.Type == "error"}
		}
		return Response{}, false, nil
	}
	for {
		line, e := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data:") {
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			data.WriteByte('\n')
		}
		if line == "" {
			r, done, err := process()
			if done || err != nil {
				return r, partial.String(), err
			}
		}
		if e != nil {
			if ctx.Err() != nil {
				return Response{}, partial.String(), ctx.Err()
			}
			return Response{}, partial.String(), fmt.Errorf("provider stream dropped: %w", e)
		}
	}
}

var toolSchemas = []Item{{"type": "function", "name": "bash", "description": "Execute a shell command in a fresh shell. Use background for long work.", "parameters": Item{"type": "object", "properties": Item{"command": Item{"type": "string"}, "justification": Item{"type": "string"}, "timeout_s": Item{"type": "integer", "minimum": 1}, "background": Item{"type": "boolean"}}, "required": []string{"command", "justification"}, "additionalProperties": false}}, {"type": "function", "name": "plan", "description": "Replace the current plan.", "parameters": Item{"type": "object", "properties": Item{"text": Item{"type": "string"}}, "required": []string{"text"}, "additionalProperties": false}}}
