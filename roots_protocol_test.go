package main

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestProtocolAllowsRootsGate pins where roots stop being askable. MCP
// 2026-07-28 (SEP-2322/2575) forbids server-initiated requests, so roots/list
// is refused from that revision on and the workspace has to arrive by header or
// tool argument instead. Getting this boundary wrong means either a pointless
// failing round-trip on every call, or dropping roots for clients that still
// answer them.
func TestProtocolAllowsRootsGate(t *testing.T) {
	for ver, want := range map[string]bool{
		"2024-11-05": true,
		"2025-06-18": true,
		"2025-11-25": true,
		"2026-07-28": false,
		"2027-03-01": false,
	} {
		if got := protocolAllowsRoots(&mcp.InitializeParams{ProtocolVersion: ver}); got != want {
			t.Errorf("protocolAllowsRoots(%s) = %v, want %v", ver, got, want)
		}
	}
	if protocolAllowsRoots(nil) {
		t.Error("nil params should not be roots-usable")
	}
}
