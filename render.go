package main

import (
	"bytes"
	"encoding/json"

	toon "github.com/toon-format/toon-go"
)

// defaultFormat is the process-wide output format for structured (JSON-shaped)
// tool results, set once from JENKINS_MCP_FORMAT at startup. "toon" or "json".
var defaultFormat = "toon"

// pickFormat resolves the output format for a single call: a per-call `format`
// arg wins, otherwise the process default. Unknown values fall back to default.
func pickFormat(args map[string]any) string {
	if f, ok := args["format"].(string); ok {
		switch f {
		case "toon", "json":
			return f
		}
	}
	return defaultFormat
}

// renderStructured serializes v as TOON (token-efficient, default) or pretty
// JSON. v is first normalized to plain maps/slices via a JSON round-trip so the
// TOON encoder sees only basic types and key order is deterministic. On any
// TOON error it falls back to pretty JSON.
func renderStructured(v any, format string) string {
	normalized := toPlain(v)
	if format == "json" {
		return marshalIndent(normalized)
	}
	s, err := toon.MarshalString(normalized)
	if err != nil {
		return marshalIndent(normalized)
	}
	return s
}

// toPlain converts an arbitrary value into plain interface{} types (map, slice,
// string, float64, bool, nil) by round-tripping through JSON, honoring the
// struct's omitempty tags so empty fields are dropped.
func toPlain(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return v
	}
	return out
}

// marshalIndent pretty-prints JSON with 2-space indent and without HTML
// escaping, matching JSON.stringify(obj, null, 2).
func marshalIndent(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "{}"
	}
	return string(bytes.TrimRight(buf.Bytes(), "\n"))
}
