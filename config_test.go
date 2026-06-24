package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func decodeRepoJobMapForTest(t *testing.T, raw map[string]any) map[string][]string {
	t.Helper()
	m := make(map[string]json.RawMessage, len(raw))
	for k, v := range raw {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		m[k] = b
	}
	return normalizeRepoJobMap(m)
}

func TestXDGConfigPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/custom/cfg")
	if got, want := xdgConfigPath(), "/custom/cfg/jenkins-mcp/config.json"; got != want {
		t.Errorf("with XDG_CONFIG_HOME: got %q, want %q", got, want)
	}

	t.Setenv("XDG_CONFIG_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	want := filepath.Join(home, ".config", "jenkins-mcp", "config.json")
	if got := xdgConfigPath(); got != want {
		t.Errorf("default: got %q, want %q", got, want)
	}
}

func TestNormalizeRepoJobMap(t *testing.T) {
	raw := map[string]any{
		"Stubbedev/Repo": "folder/job",
		"other/repo":     []any{"a/b", "c/d"},
	}
	// round-trip through JSON to mimic config decoding
	m := decodeRepoJobMapForTest(t, raw)
	if got := m["stubbedev/repo"]; len(got) != 1 || got[0] != "folder/job" {
		t.Errorf("single value: %v", got)
	}
	if got := m["other/repo"]; len(got) != 2 || got[0] != "a/b" {
		t.Errorf("array value: %v", got)
	}
}
