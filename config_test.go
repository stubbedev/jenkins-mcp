package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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
	cfg, err := loadConfig("")
	if err != nil || cfg != nil {
		t.Fatalf("no config anywhere: got %+v, %v; want disabled without error", cfg, err)
	}

	t.Setenv("JENKINS_URL", "https://env")
	t.Setenv("JENKINS_USERNAME", "eu")
	t.Setenv("JENKINS_TOKEN", "et")
	if cfg, err = loadConfig(""); err != nil || cfg == nil || cfg.URL != "https://env" {
		t.Fatalf("env only: got %+v, %v", cfg, err)
	}

	writeConfig(t, filepath.Join(dir, "xdg", "jenkins-mcp", "config.json"), validConfig)
	if cfg, err = loadConfig(""); err != nil || cfg.URL != "https://j" || cfg.RepoJobMap["org/repo"][0] != "folder/job" {
		t.Fatalf("xdg default: got %+v, %v", cfg, err)
	}

	explicit := writeConfig(t, filepath.Join(dir, "explicit.json"), `{"url": "https://explicit", "username": "x", "token": "y"}`)
	if cfg, err = loadConfig(explicit); err != nil || cfg.URL != "https://explicit" {
		t.Fatalf("--config: got %+v, %v", cfg, err)
	}
	t.Setenv("JENKINS_MCP_CONFIG", explicit)
	if cfg, err = loadConfig(""); err != nil || cfg.URL != "https://explicit" {
		t.Fatalf("JENKINS_MCP_CONFIG: got %+v, %v", cfg, err)
	}
}

func TestLoadConfigFailsLoudly(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, dir string) string
		want  []string
	}{
		{"missing JENKINS_MCP_CONFIG file", func(t *testing.T, dir string) string {
			t.Setenv("JENKINS_MCP_CONFIG", filepath.Join(dir, "nope.json"))
			return ""
		}, []string{"reading config file", "nope.json", "no such file"}},
		{"missing --config file", func(t *testing.T, dir string) string {
			return filepath.Join(dir, "gone.json")
		}, []string{"reading config file", "gone.json"}},
		{"malformed default file", func(t *testing.T, dir string) string {
			writeConfig(t, filepath.Join(dir, ".jenkins-mcp.json"), `{"url": `)
			return ""
		}, []string{"parsing config file", ".jenkins-mcp.json"}},
		{"unknown key", func(t *testing.T, dir string) string {
			writeConfig(t, filepath.Join(dir, "xdg", "jenkins-mcp", "config.json"), `{"url": "u", "tokn": "t"}`)
			return ""
		}, []string{"parsing config file", "config.json", `unknown field "tokn"`}},
		{"trailing data", func(t *testing.T, dir string) string {
			return writeConfig(t, filepath.Join(dir, "c.json"), validConfig+` {}`)
		}, []string{"c.json", "unexpected data"}},
		{"bad repoJobMap value", func(t *testing.T, dir string) string {
			return writeConfig(t, filepath.Join(dir, "c.json"), `{"repoJobMap": {"org/repo": 42}}`)
		}, []string{"c.json", `repoJobMap["org/repo"]`, "42"}},
		{"default path is a directory", func(t *testing.T, dir string) string {
			if err := os.Mkdir(filepath.Join(dir, ".jenkins-mcp.json"), 0o755); err != nil {
				t.Fatal(err)
			}
			return ""
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

func TestDotEnvFeedsEveryStartupSetting(t *testing.T) {
	dir := isolateConfigEnv(t)
	for _, k := range []string{"JENKINS_MCP_HTTP", "JENKINS_MCP_HTTP_PATH", "JENKINS_MCP_HTTP_STATELESS", "JENKINS_MCP_FORMAT", "JENKINS_URL", "JENKINS_USERNAME", "JENKINS_TOKEN"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	writeConfig(t, filepath.Join(dir, ".env"), `JENKINS_MCP_HTTP=127.0.0.1:9123
JENKINS_MCP_HTTP_PATH=/rpc
JENKINS_MCP_HTTP_STATELESS=true
JENKINS_MCP_FORMAT=json
JENKINS_URL=https://dotenv
JENKINS_USERNAME=du
JENKINS_TOKEN=dt
`)
	got, err := loadStartupSettings(cliOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := transportConfig{http: true, addr: "127.0.0.1:9123", path: "/rpc", stateless: true}
	if got.transport != want || got.format != formatJSON || got.config == nil || got.config.URL != "https://dotenv" {
		t.Errorf("settings = %+v (config %+v); want transport %+v, format json and the .env Jenkins URL", got, got.config, want)
	}
}

func TestParseDotEnv(t *testing.T) {
	got, err := parseDotEnv("x.env", strings.NewReader("# comment\n\nA=1\nexport B = two words \nC=\"quoted # not a comment\"  # trailing\nD='single'\nE=\nF=a=b\nG=tok   # optional\nH=a#b\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []envEntry{{"A", "1"}, {"B", "two words"}, {"C", "quoted # not a comment"}, {"D", "single"}, {"E", ""}, {"F", "a=b"}, {"G", "tok"}, {"H", "a#b"}}
	if !slices.Equal(got, want) {
		t.Errorf("parseDotEnv = %+v, want %+v", got, want)
	}

	for body, wantErr := range map[string]string{
		"A=1\nJENKINS_TOKEN\n": `x.env:2: expected KEY=value, got "JENKINS_TOKEN"`,
		"1A=x\n":               `x.env:1: invalid variable name "1A"`,
		"A=1\n\nA=2\n":         "x.env:3: A is already set on line 1",
		"A=\"open\n":           `x.env:1: A: unterminated " quote`,
		"A='mixed\"\n":         "x.env:1: A: unterminated ' quote",
		"=value\n":             `x.env:1: invalid variable name ""`,
	} {
		if _, err := parseDotEnv("x.env", strings.NewReader(body)); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("parseDotEnv(%q): want error containing %q, got %v", body, wantErr, err)
		}
	}
}

func TestLoadDotEnvKeepsExistingEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, filepath.Join(dir, ".env"), "JMCP_TEST_SET=fromfile\nJMCP_TEST_EMPTY=fromfile\nJMCP_TEST_NEW=fromfile\n")
	t.Setenv("JMCP_TEST_SET", "fromenv")
	t.Setenv("JMCP_TEST_EMPTY", "")
	t.Setenv("JMCP_TEST_NEW", "")
	os.Unsetenv("JMCP_TEST_NEW")
	if err := loadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"JMCP_TEST_SET": "fromenv", "JMCP_TEST_EMPTY": "", "JMCP_TEST_NEW": "fromfile"} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if err := loadDotEnv(filepath.Join(dir, "missing.env")); err != nil {
		t.Errorf("a missing .env must be optional, got %v", err)
	}
	if err := loadDotEnv(dir); err == nil || !strings.Contains(err.Error(), dir) {
		t.Errorf("an unreadable .env must fail with its path, got %v", err)
	}
}

func TestNormalizeRepoJobMapRejectsAmbiguousEntries(t *testing.T) {
	for name, c := range map[string]struct {
		raw  map[string]any
		want string
	}{
		"case-insensitive duplicate": {map[string]any{"Org/Repo": "a", "org/repo": "b"}, `repoJobMap keys "Org/Repo" and "org/repo" are the same remote substring`},
		"empty key":                  {map[string]any{" ": "a"}, "empty key, which would match every remote"},
		"empty list":                 {map[string]any{"r": []any{}}, `repoJobMap["r"] must name at least one job`},
		"bad job path":               {map[string]any{"r": []any{"a//b"}}, `repoJobMap["r"] has an invalid job path "a//b"`},
		"null value":                 {map[string]any{"r": nil}, `repoJobMap["r"] must be a job path or an array of job paths, got null`},
	} {
		m := make(map[string]json.RawMessage, len(c.raw))
		for k, v := range c.raw {
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			m[k] = b
		}
		if _, err := normalizeRepoJobMap(m); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got %v", name, c.want, err)
		}
	}
}

func TestJobPathPatternMatchesTheSchema(t *testing.T) {
	for tool, s := range toolSchemas() {
		for _, arg := range []string{"jobPath", "job", "folder"} {
			if p, ok := s.properties[arg]; ok && (p.pattern == nil || p.pattern.String() != validJobPath.String()) {
				t.Errorf("%s.%s pattern = %v, want %s", tool, arg, p.pattern, validJobPath)
			}
		}
	}
}
