package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	out, err := normalizeRepoJobMap(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
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

func isolateConfigEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg"))
	for _, k := range []string{"JENKINS_MCP_CONFIG", "JENKINS_URL", "JENKINS_USERNAME", "JENKINS_TOKEN", "JENKINS_NOTIFY_COMMIT_TOKEN"} {
		t.Setenv(k, "")
	}
	return dir
}

func writeConfig(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const validConfig = `{"$schema": "x", "url": "https://j", "username": "u", "token": "t", "repoJobMap": {"Org/Repo": "folder/job"}}`

func TestLoadConfigSources(t *testing.T) {
	dir := isolateConfigEnv(t)
	cfg, err := loadConfig(nil)
	if err != nil || cfg != nil {
		t.Fatalf("no config anywhere: got %+v, %v; want disabled without error", cfg, err)
	}

	t.Setenv("JENKINS_URL", "https://env")
	t.Setenv("JENKINS_USERNAME", "eu")
	t.Setenv("JENKINS_TOKEN", "et")
	if cfg, err = loadConfig(nil); err != nil || cfg == nil || cfg.URL != "https://env" {
		t.Fatalf("env only: got %+v, %v", cfg, err)
	}

	writeConfig(t, filepath.Join(dir, "xdg", "jenkins-mcp", "config.json"), validConfig)
	if cfg, err = loadConfig(nil); err != nil || cfg.URL != "https://j" || cfg.RepoJobMap["org/repo"][0] != "folder/job" {
		t.Fatalf("xdg default: got %+v, %v", cfg, err)
	}

	explicit := writeConfig(t, filepath.Join(dir, "explicit.json"), `{"url": "https://explicit", "username": "x", "token": "y"}`)
	if cfg, err = loadConfig([]string{"--config", explicit}); err != nil || cfg.URL != "https://explicit" {
		t.Fatalf("--config: got %+v, %v", cfg, err)
	}
	t.Setenv("JENKINS_MCP_CONFIG", explicit)
	if cfg, err = loadConfig(nil); err != nil || cfg.URL != "https://explicit" {
		t.Fatalf("JENKINS_MCP_CONFIG: got %+v, %v", cfg, err)
	}
}

func TestLoadConfigFailsLoudly(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, dir string) []string
		want  []string
	}{
		{"missing JENKINS_MCP_CONFIG file", func(t *testing.T, dir string) []string {
			t.Setenv("JENKINS_MCP_CONFIG", filepath.Join(dir, "nope.json"))
			return nil
		}, []string{"reading config file", "nope.json", "no such file"}},
		{"missing --config file", func(t *testing.T, dir string) []string {
			return []string{"--config", filepath.Join(dir, "gone.json")}
		}, []string{"reading config file", "gone.json"}},
		{"--config without a path", func(t *testing.T, dir string) []string {
			return []string{"--config"}
		}, []string{"--config needs a path"}},
		{"malformed default file", func(t *testing.T, dir string) []string {
			writeConfig(t, filepath.Join(dir, ".jenkins-mcp.json"), `{"url": `)
			return nil
		}, []string{"parsing config file", ".jenkins-mcp.json"}},
		{"unknown key", func(t *testing.T, dir string) []string {
			writeConfig(t, filepath.Join(dir, "xdg", "jenkins-mcp", "config.json"), `{"url": "u", "tokn": "t"}`)
			return nil
		}, []string{"parsing config file", "config.json", `unknown field "tokn"`}},
		{"trailing data", func(t *testing.T, dir string) []string {
			return []string{"--config", writeConfig(t, filepath.Join(dir, "c.json"), validConfig+` {}`)}
		}, []string{"c.json", "unexpected data"}},
		{"bad repoJobMap value", func(t *testing.T, dir string) []string {
			return []string{"--config", writeConfig(t, filepath.Join(dir, "c.json"), `{"repoJobMap": {"org/repo": 42}}`)}
		}, []string{"c.json", `repoJobMap["org/repo"]`, "42"}},
		{"default path is a directory", func(t *testing.T, dir string) []string {
			if err := os.Mkdir(filepath.Join(dir, ".jenkins-mcp.json"), 0o755); err != nil {
				t.Fatal(err)
			}
			return nil
		}, []string{"reading config file", ".jenkins-mcp.json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := isolateConfigEnv(t)
			cfg, err := loadConfig(tc.setup(t, dir))
			if err == nil {
				t.Fatalf("want an error, got config %+v", cfg)
			}
			for _, s := range tc.want {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q missing %q", err, s)
				}
			}
		})
	}
}
