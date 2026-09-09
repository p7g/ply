package ply

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

type Item map[string]any

func str(v any) string { s, _ := v.(string); return s }
func num(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}
func message(role, text string) Item {
	typ := "input_text"
	if role == "assistant" {
		typ = "output_text"
	}
	return Item{"type": "message", "role": role, "content": []any{Item{"type": typ, "text": text}}}
}
func textOf(i Item) string {
	if s, ok := i["content"].(string); ok {
		return s
	}
	var b strings.Builder
	a, _ := i["content"].([]any)
	if a == nil {
		a, _ = i["summary"].([]any)
	}
	for _, v := range a {
		if m, ok := v.(map[string]any); ok {
			b.WriteString(str(m["text"]))
		} else if m, ok := v.(Item); ok {
			b.WriteString(str(m["text"]))
		}
	}
	return b.String()
}
func clean(i Item) Item {
	r := Item{}
	for k, v := range i {
		if k != "seq" && k != "ts" && !strings.HasPrefix(k, "ply.") {
			r[k] = v
		}
	}
	return r
}
func readItems(path string, following bool) ([]Item, error) {
	f, e := os.Open(path)
	if os.IsNotExist(e) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	defer f.Close()
	return decodeItems(f, following)
}
func decodeItems(r io.Reader, following bool) ([]Item, error) {
	br := bufio.NewReader(r)
	var items []Item
	for {
		line, e := br.ReadBytes('\n')
		if e == io.EOF && len(line) == 0 {
			break
		}
		if e == io.EOF && following {
			break
		}
		if e != nil && e != io.EOF {
			return nil, e
		}
		if len(line) == 0 || line[len(line)-1] != '\n' {
			return nil, fmt.Errorf("transcript has an incomplete final line; preserve and repair it before writing")
		}
		if !utf8.Valid(line) {
			return nil, fmt.Errorf("invalid UTF-8 at line %d", len(items)+1)
		}
		var i Item
		d := json.NewDecoder(bytes.NewReader(line))
		d.UseNumber()
		if err := d.Decode(&i); err != nil {
			return nil, fmt.Errorf("line %d: %w", len(items)+1, err)
		}
		seq, seqOK := i["seq"].(json.Number)
		index, seqErr := seq.Int64()
		_, timeErr := time.Parse(time.RFC3339Nano, str(i["ts"]))
		if i == nil || !seqOK || seqErr != nil || index != int64(len(items)) || timeErr != nil || str(i["type"]) == "" {
			return nil, fmt.Errorf("invalid envelope at line %d", len(items)+1)
		}
		var extra any
		if d.Decode(&extra) != io.EOF {
			return nil, fmt.Errorf("extra data at line %d", len(items)+1)
		}
		items = append(items, i)
	}
	if len(items) > 0 && (str(items[0]["type"]) != "ply.meta" || num(items[0]["version"]) != 1) {
		return nil, fmt.Errorf("invalid or unsupported ply.meta")
	}
	return items, nil
}

type Transcript struct {
	File     *os.File
	Items    []Item
	OnAppend func(Item)
}

func openTranscript(path, cwd string) (*Transcript, error) {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, fmt.Errorf("transcript is locked by another ply invocation: %s", path)
	}
	items, e := decodeItems(f, false)
	if e != nil {
		f.Close()
		return nil, e
	}
	t := &Transcript{File: f, Items: items}
	if len(items) == 0 {
		e = t.Append(Item{"type": "ply.meta", "version": 1, "cwd": cwd, "created": time.Now().UTC().Format(time.RFC3339), "ply_version": Version})
	}
	if e != nil {
		f.Close()
		return nil, e
	}
	return t, nil
}
func (t *Transcript) Close() { t.File.Close() }
func (t *Transcript) Append(i Item) error {
	i["seq"] = len(t.Items)
	i["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	b, e := json.Marshal(i)
	if e != nil {
		return e
	}
	b = append(b, '\n')
	n, e := t.File.Write(b)
	if e != nil {
		return e
	}
	if n != len(b) {
		return io.ErrShortWrite
	}
	if e = t.File.Sync(); e != nil {
		return e
	}
	t.Items = append(t.Items, i)
	if t.OnAppend != nil {
		t.OnAppend(i)
	}
	return nil
}
func latest(items []Item, typ string) Item {
	for n := len(items) - 1; n >= 0; n-- {
		if str(items[n]["type"]) == typ {
			return items[n]
		}
	}
	return nil
}
func Replay(items []Item) []Item {
	out := []Item{}
	if i := latest(items, "ply.system"); i != nil {
		out = append(out, message("system", str(i["text"])))
	}
	if i := latest(items, "ply.plan"); i != nil {
		out = append(out, message("developer", "Current plan:\n"+str(i["text"])))
	}
	c := -1
	for n, i := range items {
		if str(i["type"]) == "ply.compaction" {
			c = n
		}
	}
	if c >= 0 && items[c]["summary"] != nil {
		out = append(out, message("user", str(items[c]["summary"])))
	}
	for _, i := range items[c+1:] {
		typ := str(i["type"])
		if i["ply.mode"] != nil {
			continue
		}
		switch typ {
		case "ply.task_done":
			out = append(out, message("user", fmt.Sprintf("[task %v finished: exit %v in %vs]\n%s", i["task"], i["exit_code"], i["duration_s"], str(i["output_tail"]))))
		case "ply.interrupt":
			out = append(out, message("user", "[interrupted by user]"))
		default:
			if !strings.HasPrefix(typ, "ply.") {
				out = append(out, clean(i))
			}
		}
	}
	return out
}
func pending(items []Item) []Item {
	done := map[string]bool{}
	for _, i := range items {
		if str(i["type"]) == "function_call_output" {
			done[str(i["call_id"])] = true
		}
	}
	var out []Item
	for _, i := range items {
		if str(i["type"]) == "function_call" && !done[str(i["call_id"])] {
			out = append(out, i)
		}
	}
	return out
}
