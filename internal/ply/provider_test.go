package ply

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderStreamingAndRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Error(r.URL)
		}
		var body Item
		json.NewDecoder(r.Body).Decode(&body)
		if body["store"] != false || body["stream"] != true || body["tools"] == nil {
			t.Error(body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n")
	}))
	defer srv.Close()
	p := &Provider{Config: Config{Values: map[string]any{"model": "test", "base_url": srv.URL + "/v1", "provider.retries": 0}}, Client: srv.Client()}
	var s strings.Builder
	r, partial, e := p.Call(context.Background(), []Item{message("user", "hi")}, true, func(d string) { s.WriteString(d) })
	if e != nil || partial != "hello" || s.String() != "hello" || r.Usage.Input != 5 {
		t.Fatal(r, partial, e)
	}
}
func TestProviderRetryAndPermanentError(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if count.Add(1) == 1 {
			w.WriteHeader(429)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"output":[],"status":"completed"}`)
	}))
	defer srv.Close()
	p := &Provider{Config: Config{Values: map[string]any{"model": "test", "base_url": srv.URL, "provider.retries": 1}}, Client: srv.Client()}
	if _, _, e := p.Call(context.Background(), nil, false, nil); e != nil || count.Load() != 2 {
		t.Fatal(count.Load(), e)
	}
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) })
	start := time.Now()
	if _, _, e := p.Call(context.Background(), nil, false, nil); e == nil || time.Since(start) > 500*time.Millisecond {
		t.Fatal(e)
	}
}
func TestProviderDroppedStreamAndCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
	}))
	defer srv.Close()
	p := &Provider{Config: Config{Values: map[string]any{"model": "test", "base_url": srv.URL, "provider.retries": 0}}, Client: srv.Client()}
	r, _, e := p.Call(context.Background(), nil, false, nil)
	if e == nil || len(r.Output) != 0 {
		t.Fatal(r, e)
	}
}

func TestReplayProviderIncludesCompactionSummary(t *testing.T) {
	path := t.TempDir() + "/recorded.jsonl"
	fixture(t, path, message("assistant", "first"), Item{"type": "ply.usage", "input_tokens": 90, "output_tokens": 1}, Item{"type": "ply.usage", "input_tokens": 95, "output_tokens": 4}, Item{"type": "ply.compaction", "summary": "preserved request"}, message("assistant", "last"), Item{"type": "ply.usage", "input_tokens": 5, "output_tokens": 1})
	p, e := newProvider(Config{}, "replay:"+path)
	if e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{"first", "preserved request", "last"} {
		r, _, e := p.Call(context.Background(), nil, false, nil)
		if e != nil || len(r.Output) != 1 || textOf(r.Output[0]) != want {
			t.Fatal(r, e)
		}
	}
}
