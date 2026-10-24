package main

import (
	"bytes"
	"encoding/json"
	"fmt"

	toon "github.com/toon-format/toon-go"
)

type outputFormat string

const (
	formatTOON outputFormat = "toon"
	formatJSON outputFormat = "json"
)

var defaultFormat = formatTOON

func parseOutputFormat(s string) (outputFormat, error) {
	switch f := outputFormat(s); f {
	case formatTOON, formatJSON:
		return f, nil
	}
	return "", fmt.Errorf("format must be %q or %q, got %q", formatTOON, formatJSON, s)
}

func pickFormat(args toolArgs) (outputFormat, error) {
	if !args.has("format") {
		return defaultFormat, nil
	}
	return parseOutputFormat(args.str("format"))
}

func renderStructured(v any, format outputFormat) (string, error) {
	normalized, err := toPlain(v)
	if err != nil {
		return "", err
	}
	switch format {
	case formatJSON:
		return marshalIndent(normalized)
	case formatTOON:
		s, err := toon.MarshalString(normalized)
		if err != nil {
			return "", fmt.Errorf("rendering result as TOON: %w", err)
		}
		return s, nil
	}
	return "", fmt.Errorf("rendering result: unknown format %q", format)
}

func toPlain(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encoding result as JSON: %w", err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("decoding result JSON: %w", err)
	}
	return out, nil
}

func marshalIndent(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "", fmt.Errorf("rendering result as JSON: %w", err)
	}
	return string(bytes.TrimRight(buf.Bytes(), "\n")), nil
}
