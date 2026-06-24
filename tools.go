package main

import (
	_ "embed"
	"encoding/json"
)

//go:embed tools.json
var toolsJSON []byte

// toolDef is a single MCP tool definition loaded from tools.json. InputSchema is
// kept as raw JSON so it is passed to the client verbatim (via RawInputSchema).
type toolDef struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	InputSchema json.RawMessage  `json:"inputSchema"`
	Annotations *toolAnnotations `json:"annotations"`
}

// toolAnnotations mirrors the MCP tool behavior hints. Pointers so an omitted
// hint stays unset rather than defaulting to false.
type toolAnnotations struct {
	ReadOnlyHint    *bool `json:"readOnlyHint"`
	DestructiveHint *bool `json:"destructiveHint"`
	IdempotentHint  *bool `json:"idempotentHint"`
	OpenWorldHint   *bool `json:"openWorldHint"`
}

func loadToolDefs() []toolDef {
	var defs []toolDef
	if err := json.Unmarshal(toolsJSON, &defs); err != nil {
		panic("invalid embedded tools.json: " + err.Error())
	}
	return defs
}
