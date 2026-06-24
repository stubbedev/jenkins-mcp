package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Config holds the resolved Jenkins connection settings plus the optional
// repo→job mapping used to auto-resolve jobPath from the current git remote.
type Config struct {
	URL        string
	Username   string
	Token      string
	RepoJobMap map[string][]string
}

// configFile mirrors the on-disk ~/.jenkins-mcp.json shape. repoJobMap values
// may be a single string or an array of strings, so it is decoded leniently.
type configFile struct {
	URL        string                     `json:"url"`
	Username   string                     `json:"username"`
	Token      string                     `json:"token"`
	RepoJobMap map[string]json.RawMessage `json:"repoJobMap"`
}

// loadConfig resolves configuration in the same precedence order as the original
// TypeScript server: --config flag → JENKINS_MCP_CONFIG → ~/.jenkins-mcp.json →
// ./.jenkins-mcp.json, with JENKINS_URL / JENKINS_USERNAME / JENKINS_TOKEN env
// vars as fallbacks for any field the file does not set. Returns nil when the
// required url/username/token are not all present.
func loadConfig() *Config {
	loadDotEnv()

	var file *configFile
	if path := configPath(); path != "" {
		file = readConfigFile(path)
	}

	get := func(fileVal, envKey string) string {
		if fileVal != "" {
			return fileVal
		}
		return os.Getenv(envKey)
	}

	cfg := &Config{}
	if file != nil {
		cfg.URL = file.URL
		cfg.Username = file.Username
		cfg.Token = file.Token
		cfg.RepoJobMap = normalizeRepoJobMap(file.RepoJobMap)
	}
	cfg.URL = get(cfg.URL, "JENKINS_URL")
	cfg.Username = get(cfg.Username, "JENKINS_USERNAME")
	cfg.Token = get(cfg.Token, "JENKINS_TOKEN")

	if cfg.URL == "" || cfg.Username == "" || cfg.Token == "" {
		var missing []string
		if cfg.URL == "" {
			missing = append(missing, "url (or JENKINS_URL)")
		}
		if cfg.Username == "" {
			missing = append(missing, "username (or JENKINS_USERNAME)")
		}
		if cfg.Token == "" {
			missing = append(missing, "token (or JENKINS_TOKEN)")
		}
		logErr("[jenkins-mcp] Disabled: missing " + strings.Join(missing, ", "))
		return nil
	}
	return cfg
}

// configPath finds the config file path using the documented precedence,
// returning "" when none is found.
func configPath() string {
	args := os.Args[1:]
	for i, a := range args {
		if a == "--config" && i+1 < len(args) {
			abs, _ := filepath.Abs(args[i+1])
			return abs
		}
	}
	if env := os.Getenv("JENKINS_MCP_CONFIG"); env != "" {
		abs, _ := filepath.Abs(env)
		return abs
	}
	if home, err := os.UserHomeDir(); err == nil {
		if dotfile := filepath.Join(home, ".jenkins-mcp.json"); fileExists(dotfile) {
			return dotfile
		}
	}
	// XDG: $XDG_CONFIG_HOME/jenkins-mcp/config.json (default ~/.config/...).
	if xdg := xdgConfigPath(); xdg != "" && fileExists(xdg) {
		return xdg
	}
	if cwd, err := os.Getwd(); err == nil {
		cwdCfg := filepath.Join(cwd, ".jenkins-mcp.json")
		if fileExists(cwdCfg) {
			return cwdCfg
		}
	}
	return ""
}

// xdgConfigPath returns the XDG config file location for jenkins-mcp, honoring
// $XDG_CONFIG_HOME and falling back to ~/.config.
func xdgConfigPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "jenkins-mcp", "config.json")
}

func readConfigFile(path string) *configFile {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f configFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil
	}
	return &f
}

// normalizeRepoJobMap lowercases the remote-substring keys and coerces each
// value (string or []string) into []string, matching the TS constructor.
func normalizeRepoJobMap(raw map[string]json.RawMessage) map[string][]string {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string][]string, len(raw))
	for k, v := range raw {
		var single string
		if err := json.Unmarshal(v, &single); err == nil {
			out[strings.ToLower(k)] = []string{single}
			continue
		}
		var list []string
		if err := json.Unmarshal(v, &list); err == nil {
			out[strings.ToLower(k)] = list
		}
	}
	return out
}

// loadDotEnv loads ./.env into the process environment (without overriding
// already-set vars), replacing the TS `dotenv/config` import. Best-effort.
func loadDotEnv() {
	f, err := os.Open(".env")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		if key != "" && os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
