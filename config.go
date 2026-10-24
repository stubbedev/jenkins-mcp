package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Config holds the resolved Jenkins connection settings plus the optional
// repo→job mapping used to auto-resolve jobPath from the current git remote.
type Config struct {
	URL               string
	Username          string
	Token             string
	RepoJobMap        map[string][]string
	NotifyCommitToken string
}

type configFile struct {
	Schema            string                     `json:"$schema"`
	URL               string                     `json:"url"`
	Username          string                     `json:"username"`
	Token             string                     `json:"token"`
	NotifyCommitToken string                     `json:"notifyCommitToken"`
	RepoJobMap        map[string]json.RawMessage `json:"repoJobMap"`
}

type configSource struct {
	path     string
	explicit bool
}

func loadConfig(args []string) (*Config, error) {
	sources, err := configSources(args)
	if err != nil {
		return nil, err
	}
	cfg, err := readConfigFile(sources)
	if err != nil {
		return nil, err
	}

	get := func(fileVal, envKey string) string {
		if fileVal != "" {
			return fileVal
		}
		return os.Getenv(envKey)
	}
	cfg.URL = get(cfg.URL, "JENKINS_URL")
	cfg.Username = get(cfg.Username, "JENKINS_USERNAME")
	cfg.Token = get(cfg.Token, "JENKINS_TOKEN")
	cfg.NotifyCommitToken = get(cfg.NotifyCommitToken, "JENKINS_NOTIFY_COMMIT_TOKEN")

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
		return nil, nil
	}
	return &cfg, nil
}

func configSources(args []string) ([]configSource, error) {
	for i, a := range args {
		if a != "--config" {
			continue
		}
		if i+1 >= len(args) || args[i+1] == "" {
			return nil, errors.New("--config needs a path argument")
		}
		return []configSource{{path: args[i+1], explicit: true}}, nil
	}
	if env := os.Getenv("JENKINS_MCP_CONFIG"); env != "" {
		return []configSource{{path: env, explicit: true}}, nil
	}
	var out []configSource
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, configSource{path: filepath.Join(home, ".jenkins-mcp.json")})
	}
	if xdg := xdgConfigPath(); xdg != "" {
		out = append(out, configSource{path: xdg})
	}
	return append(out, configSource{path: ".jenkins-mcp.json"}), nil
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

func readConfigFile(sources []configSource) (Config, error) {
	for _, src := range sources {
		data, err := os.ReadFile(src.path)
		if err != nil {
			if !src.explicit && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return Config{}, fmt.Errorf("reading config file: %w", err)
		}
		cfg, err := parseConfigFile(data)
		if err != nil {
			return Config{}, fmt.Errorf("parsing config file %s: %w", src.path, err)
		}
		return cfg, nil
	}
	return Config{}, nil
}

func parseConfigFile(data []byte) (Config, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f configFile
	if err := dec.Decode(&f); err != nil {
		return Config{}, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("unexpected data after the top-level JSON object")
	}
	repoJobMap, err := normalizeRepoJobMap(f.RepoJobMap)
	if err != nil {
		return Config{}, err
	}
	return Config{URL: f.URL, Username: f.Username, Token: f.Token, NotifyCommitToken: f.NotifyCommitToken, RepoJobMap: repoJobMap}, nil
}

func normalizeRepoJobMap(raw map[string]json.RawMessage) (map[string][]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string][]string, len(raw))
	for _, k := range slices.Sorted(maps.Keys(raw)) {
		var single string
		if err := json.Unmarshal(raw[k], &single); err == nil {
			out[strings.ToLower(k)] = []string{single}
			continue
		}
		var list []string
		if err := json.Unmarshal(raw[k], &list); err != nil {
			return nil, fmt.Errorf("repoJobMap[%q] must be a job path or an array of job paths, got %s", k, raw[k])
		}
		out[strings.ToLower(k)] = list
	}
	return out, nil
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
