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
	"regexp"
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

func loadConfig(explicitPath string) (*Config, error) {
	cfg, err := readConfigFile(configSources(explicitPath))
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

func configSources(explicitPath string) []configSource {
	if explicitPath != "" {
		return []configSource{{path: explicitPath, explicit: true}}
	}
	if env := os.Getenv("JENKINS_MCP_CONFIG"); env != "" {
		return []configSource{{path: env, explicit: true}}
	}
	var out []configSource
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, configSource{path: filepath.Join(home, ".jenkins-mcp.json")})
	}
	if xdg := xdgConfigPath(); xdg != "" {
		out = append(out, configSource{path: xdg})
	}
	return append(out, configSource{path: ".jenkins-mcp.json"})
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

var validJobPath = regexp.MustCompile(`^[^/]+(/[^/]+)*$`)

func normalizeRepoJobMap(raw map[string]json.RawMessage) (map[string][]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string][]string, len(raw))
	firstKey := make(map[string]string, len(raw))
	for _, k := range slices.Sorted(maps.Keys(raw)) {
		needle := strings.ToLower(strings.TrimSpace(k))
		if needle == "" {
			return nil, errors.New("repoJobMap has an empty key, which would match every remote")
		}
		if prev, dup := firstKey[needle]; dup {
			return nil, fmt.Errorf("repoJobMap keys %q and %q are the same remote substring (keys are case-insensitive)", prev, k)
		}
		firstKey[needle] = k
		jobs, err := decodeJobPaths(raw[k])
		if err != nil {
			return nil, fmt.Errorf("repoJobMap[%q] %w", k, err)
		}
		out[needle] = jobs
	}
	return out, nil
}

func decodeJobPaths(raw json.RawMessage) ([]string, error) {
	value, err := decodeJSONValue(raw)
	if err != nil {
		return nil, fmt.Errorf("is not valid JSON: %w", err)
	}
	var list []string
	switch v := value.(type) {
	case string:
		list = []string{v}
	case []any:
		for _, e := range v {
			job, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("must be a job path or an array of job paths, got %s", raw)
			}
			list = append(list, job)
		}
	default:
		return nil, fmt.Errorf("must be a job path or an array of job paths, got %s", raw)
	}
	if len(list) == 0 {
		return nil, errors.New("must name at least one job")
	}
	for _, job := range list {
		if !validJobPath.MatchString(job) {
			return nil, fmt.Errorf("has an invalid job path %q (folders are separated by single slashes, without leading or trailing ones)", job)
		}
	}
	return list, nil
}

const dotEnvFile = ".env"

var dotEnvKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type envEntry struct {
	key   string
	value string
}

func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading env file: %w", err)
	}
	defer f.Close()
	entries, err := parseDotEnv(path, f)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if _, set := os.LookupEnv(e.key); set {
			continue
		}
		if err := os.Setenv(e.key, e.value); err != nil {
			return fmt.Errorf("%s: setting %s: %w", path, e.key, err)
		}
	}
	return nil
}

func parseDotEnv(name string, r io.Reader) ([]envEntry, error) {
	var entries []envEntry
	seen := map[string]int{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=value, got %q", name, n, line)
		}
		key = strings.TrimSpace(key)
		if !dotEnvKey.MatchString(key) {
			return nil, fmt.Errorf("%s:%d: invalid variable name %q", name, n, key)
		}
		if first, dup := seen[key]; dup {
			return nil, fmt.Errorf("%s:%d: %s is already set on line %d", name, n, key, first)
		}
		seen[key] = n
		value, err := unquoteDotEnvValue(strings.TrimSpace(val))
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %s: %w", name, n, key, err)
		}
		entries = append(entries, envEntry{key: key, value: value})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	return entries, nil
}

func unquoteDotEnvValue(v string) (string, error) {
	if v == "" || (v[0] != '"' && v[0] != '\'') {
		if i := strings.IndexByte(v, '#'); i == 0 || (i > 0 && (v[i-1] == ' ' || v[i-1] == '\t')) {
			v = strings.TrimSpace(v[:i])
		}
		return v, nil
	}
	end := strings.IndexByte(v[1:], v[0])
	if end < 0 {
		return "", fmt.Errorf("unterminated %c quote in %s", v[0], v)
	}
	if rest := strings.TrimSpace(v[end+2:]); rest != "" && !strings.HasPrefix(rest, "#") {
		return "", fmt.Errorf("unexpected %q after the closing quote", rest)
	}
	return v[1 : end+1], nil
}
