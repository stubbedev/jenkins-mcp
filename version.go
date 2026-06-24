package main

import (
	_ "embed"
	"encoding/json"
)

// packageJSON is embedded so the binary's version is sourced from the single
// source of truth — package.json. A `npm run release:patch` bump therefore
// propagates to every build path (local `go build`, `go install`, CI, Nix, and
// the release workflow) with no ldflags or codegen required.
//
//go:embed package.json
var packageJSON []byte

// Version is the server version, read from the embedded package.json at startup.
var Version = func() string {
	var p struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(packageJSON, &p); err == nil && p.Version != "" {
		return p.Version
	}
	return "dev"
}()

// userAgent is sent on every Jenkins request — identifies the client and avoids
// the Go default ("Go-http-client/..."), which some WAFs/proxies reject.
var userAgent = "jenkins-mcp/" + Version
