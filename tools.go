package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

//go:embed tools.json
var toolsJSON []byte

type toolDef struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	InputSchema json.RawMessage  `json:"inputSchema"`
	Annotations *toolAnnotations `json:"annotations"`
	schema      *schema
}

type toolAnnotations struct {
	ReadOnlyHint    *bool `json:"readOnlyHint"`
	DestructiveHint *bool `json:"destructiveHint"`
	IdempotentHint  *bool `json:"idempotentHint"`
	OpenWorldHint   *bool `json:"openWorldHint"`
}

func parseToolDefs(data []byte) ([]toolDef, error) {
	var defs []toolDef
	if err := json.Unmarshal(data, &defs); err != nil {
		return nil, err
	}
	for i := range defs {
		raw, err := decodeJSONValue(defs[i].InputSchema)
		if err != nil {
			return nil, fmt.Errorf("%s: input schema: %w", defs[i].Name, err)
		}
		if defs[i].schema, err = compileSchema(defs[i].Name, raw); err != nil {
			return nil, err
		}
	}
	return defs, nil
}

var loadToolDefs = sync.OnceValue(func() []toolDef {
	defs, err := parseToolDefs(toolsJSON)
	if err != nil {
		panic("invalid embedded tools.json: " + err.Error())
	}
	return defs
})

var toolSchemas = sync.OnceValue(func() map[string]*schema {
	out := make(map[string]*schema, len(loadToolDefs()))
	for _, d := range loadToolDefs() {
		out[d.Name] = d.schema
	}
	return out
})
