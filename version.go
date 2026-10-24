package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
)

// packageJSON is embedded so the binary's version is sourced from the single
// source of truth — package.json. A `npm run release:patch` bump therefore
// propagates to every build path (local `go build`, `go install`, CI, Nix, and
// the release workflow) with no ldflags or codegen required.
//
//go:embed package.json
var packageJSON []byte

var Version = mustPackageVersion(packageJSON)

func mustPackageVersion(data []byte) string {
	v, err := packageVersion(data)
	if err != nil {
		panic("embedded package.json: " + err.Error())
	}
	return v
}

func packageVersion(data []byte) (string, error) {
	var p struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return "", err
	}
	if !semverCore.MatchString(p.Version) {
		return "", fmt.Errorf("version %q is not a semantic version", p.Version)
	}
	return p.Version, nil
}

var semverCore = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// userAgent is sent on every Jenkins request — identifies the client and avoids
// the Go default ("Go-http-client/..."), which some WAFs/proxies reject.
var userAgent = "jenkins-mcp/" + Version
