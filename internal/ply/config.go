package ply

import (
	"fmt"
	"github.com/pelletier/go-toml/v2"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Values  map[string]any
	Sources map[string]string
	Project string
}

var defaults = map[string]any{
	"model": "", "api_key": "", "base_url": "https://api.openai.com/v1",
	"context_window": 0, "compact_at": 0.8,
	"detach": false, "pager": true, "system_file": "",
	"provider.retries": 5,
	"approve.command":  "ply-approve-chain ply-approve-allowlist ply-approve-ask",
	"approve.model":    "", "approve.context_window": 0,
	"bash.shell": "/bin/bash", "bash.default_timeout": 120, "bash.max_output_lines": 200,
	"output.max_lines": 200, "output.color": "auto", "output.show_thinking": false, "output.show_usage": true,
}

func (c Config) S(k string) string { return str(c.Values[k]) }
func (c Config) N(k string) int    { return num(c.Values[k]) }
func (c Config) B(k string) bool   { b, _ := c.Values[k].(bool); return b }
func (c Config) Threshold() int {
	v := c.Values["compact_at"]
	n, ok := v.(float64)
	if !ok {
		n = float64(num(v))
	}
	if n < 1 {
		return int(n * float64(c.N("context_window")))
	}
	return int(n)
}
func userDir() string {
	if s := os.Getenv("XDG_CONFIG_HOME"); s != "" {
		return filepath.Join(s, "ply")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config", "ply")
}
func ancestors(cwd string) []string {
	var a []string
	for {
		a = append(a, cwd)
		p := filepath.Dir(cwd)
		if p == cwd {
			break
		}
		cwd = p
	}
	return a
}
func projectDir(cwd string) string {
	for _, d := range ancestors(cwd) {
		p := filepath.Join(d, ".ply")
		if s, e := os.Stat(p); e == nil && s.IsDir() {
			return p
		}
	}
	return filepath.Join(cwd, ".ply")
}
func flatten(m map[string]any, p string, out map[string]any) {
	for k, v := range m {
		key := p + k
		if sub, ok := v.(map[string]any); ok {
			flatten(sub, key+".", out)
		} else {
			out[key] = v
		}
	}
}
func loadTOML(path string) (map[string]any, error) {
	b, e := os.ReadFile(path)
	if os.IsNotExist(e) {
		return map[string]any{}, nil
	}
	if e != nil {
		return nil, e
	}
	m := map[string]any{}
	if e = toml.Unmarshal(b, &m); e != nil {
		return nil, fmt.Errorf("%s: %w", path, e)
	}
	r := map[string]any{}
	flatten(m, "", r)
	return r, nil
}
func sensitive(k string) bool {
	return k == "api_key" || k == "model" || k == "base_url" || k == "bash.shell" || strings.HasPrefix(k, "provider.") || strings.HasPrefix(k, "approve.")
}
func resolve(cwd string, o Options) (Config, error) {
	c := Config{Values: map[string]any{}, Sources: map[string]string{}, Project: projectDir(cwd)}
	for k, v := range defaults {
		c.Values[k] = v
		c.Sources[k] = "default"
	}
	up := filepath.Join(userDir(), "config.toml")
	u, e := loadTOML(up)
	if e != nil {
		return c, e
	}
	trusted := o.Trust
	root := filepath.Dir(c.Project)
	if a, ok := u["trust"].([]any); ok {
		for _, v := range a {
			p, _ := filepath.Abs(str(v))
			rp, _ := filepath.EvalSymlinks(p)
			rr, _ := filepath.EvalSymlinks(root)
			if p == root || (rp != "" && rp == rr) {
				trusted = true
			}
		}
	}
	apply := func(m map[string]any, source string, project bool) error {
		for k, v := range m {
			if k == "trust" {
				continue
			}
			if _, ok := defaults[k]; !ok {
				return fmt.Errorf("%s: unknown configuration key %s", source, k)
			}
			if project && sensitive(k) && !trusted {
				fmt.Fprintf(os.Stderr, "ply: ignoring untrusted project key %s in %s\n", k, source)
				continue
			}
			c.Values[k] = v
			c.Sources[k] = source
		}
		return nil
	}
	if e = apply(u, up, false); e != nil {
		return c, e
	}
	pp := filepath.Join(c.Project, "config.toml")
	if o.Config != "" {
		pp = o.Config
		if _, e = os.Stat(pp); e != nil {
			return c, e
		}
	}
	p, e := loadTOML(pp)
	if e != nil {
		return c, e
	}
	if e = apply(p, pp, true); e != nil {
		return c, e
	}
	for k, d := range defaults {
		if s, ok := os.LookupEnv("PLY_" + strings.ToUpper(strings.ReplaceAll(k, ".", "_"))); ok {
			v, e := parseValue(s, d)
			if e != nil {
				return c, fmt.Errorf("%s: %w", k, e)
			}
			c.Values[k] = v
			c.Sources[k] = "env"
		}
	}
	for k, s := range o.Overrides {
		v, e := parseValue(s, defaults[k])
		if e != nil {
			return c, fmt.Errorf("--%s: %w", strings.ReplaceAll(k, ".", "-"), e)
		}
		c.Values[k] = v
		c.Sources[k] = "flag"
	}
	for k, d := range defaults {
		v := c.Values[k]
		switch d.(type) {
		case string:
			if _, ok := v.(string); !ok {
				return c, fmt.Errorf("%s must be a string", k)
			}
		case bool:
			if _, ok := v.(bool); !ok {
				return c, fmt.Errorf("%s must be boolean", k)
			}
		case int:
			switch v.(type) {
			case int, int64:
			default:
				return c, fmt.Errorf("%s must be an integer", k)
			}
		case float64:
			switch v.(type) {
			case int, int64, float64:
			default:
				return c, fmt.Errorf("%s must be a number", k)
			}
		}
	}
	if c.N("provider.retries") < 0 || c.N("bash.default_timeout") <= 0 || c.N("output.max_lines") < 2 || c.N("bash.max_output_lines") < 2 || c.N("approve.context_window") < 0 || c.N("context_window") < 0 {
		return c, fmt.Errorf("invalid retries, timeout, output limits, or context window")
	}
	f := c.Values["compact_at"]
	fv, _ := strconv.ParseFloat(fmt.Sprint(f), 64)
	if fv <= 0 || math.IsNaN(fv) || math.IsInf(fv, 0) {
		return c, fmt.Errorf("compact_at must be positive and finite")
	}
	switch c.S("output.color") {
	case "auto", "always", "never":
	default:
		return c, fmt.Errorf("output.color must be auto, always, or never")
	}
	return c, nil
}
func parseValue(s string, d any) (any, error) {
	switch d.(type) {
	case bool:
		return strconv.ParseBool(s)
	case int:
		return strconv.Atoi(s)
	case float64:
		return strconv.ParseFloat(s, 64)
	default:
		return s, nil
	}
}
func (c Config) require() error {
	for _, k := range []string{"model", "context_window"} {
		if fmt.Sprint(c.Values[k]) == "" || fmt.Sprint(c.Values[k]) == "0" {
			return fmt.Errorf("required configuration %s missing: set --%s, PLY_%s, or user config %s", k, strings.ReplaceAll(k, "_", "-"), strings.ToUpper(k), filepath.Join(userDir(), "config.toml"))
		}
	}
	return nil
}

// snapshot contains only non-secret configuration.
func (c Config) snapshot() map[string]any {
	out := map[string]any{}
	for k, v := range c.Values {
		if k != "api_key" {
			out[k] = v
		}
	}
	return out
}
