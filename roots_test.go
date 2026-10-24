package main

import (
	"strings"
	"testing"
)

func TestFileURIToPath(t *testing.T) {
	for in, want := range map[string]string{
		"file:///home/me/repo":      "/home/me/repo",
		"file:///tmp/with%20space":  "/tmp/with space",
		"file://localhost/srv/repo": "/srv/repo",
	} {
		if got, local, err := fileURIToPath(in); err != nil || !local || got != want {
			t.Errorf("fileURIToPath(%q) = %q, %v, %v; want %q", in, got, local, err, want)
		}
	}
	for _, in := range []string{"https://example.com/x", "vscode://file/x"} {
		if got, local, err := fileURIToPath(in); err != nil || local {
			t.Errorf("fileURIToPath(%q) = %q, %v, %v; want a non-local root and no error", in, got, local, err)
		}
	}
	for in, want := range map[string]string{
		"/already/a/path":       "bare path",
		"":                      "bare path",
		"file://server/share/x": `host "server"`,
		"file://":               "no path",
		"file://%zz/x":          "invalid URL escape",
	} {
		if _, _, err := fileURIToPath(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("fileURIToPath(%q): want error containing %q, got %v", in, want, err)
		}
	}
}
