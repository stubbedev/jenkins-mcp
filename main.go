package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

var jenkins *JenkinsClient

func main() {
	for _, a := range os.Args[1:] {
		if a == "--version" || a == "-v" {
			fmt.Println("jenkins-mcp " + Version)
			return
		}
	}

	if f := strings.ToLower(os.Getenv("JENKINS_MCP_FORMAT")); f == "json" || f == "toon" {
		defaultFormat = f
	}

	config := loadConfig()
	if config != nil {
		jenkins = NewJenkinsClient(config.URL, config.Username, config.Token, config.RepoJobMap)
	}

	s := server.NewMCPServer(
		"jenkins-mcp", Version,
		server.WithToolCapabilities(false),
		server.WithInstructions(buildInstructions(config)),
	)

	for _, def := range loadToolDefs() {
		tool := mcp.Tool{
			Name:           def.Name,
			Description:    def.Description,
			RawInputSchema: def.InputSchema,
		}
		if a := def.Annotations; a != nil {
			tool.Annotations = mcp.ToolAnnotation{
				ReadOnlyHint:    a.ReadOnlyHint,
				DestructiveHint: a.DestructiveHint,
				IdempotentHint:  a.IdempotentHint,
				OpenWorldHint:   a.OpenWorldHint,
			}
		}
		name := def.Name
		s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return dispatch(ctx, name, req.GetArguments())
		})
	}

	if err := server.ServeStdio(s); err != nil {
		logErr("[jenkins-mcp] fatal: " + err.Error())
		os.Exit(1)
	}
}

// dispatch routes a tool call to the matching client method and wraps the result.
func dispatch(ctx context.Context, name string, rawArgs map[string]any) (*mcp.CallToolResult, error) {
	if jenkins == nil {
		return mcp.NewToolResultError("Jenkins is not configured. Set url + username + token in ~/.jenkins-mcp.json or via JENKINS_URL / JENKINS_USERNAME / JENKINS_TOKEN env vars."), nil
	}
	args := normalizeArgs(rawArgs)
	out, err := runTool(ctx, name, args)
	if err != nil {
		return mcp.NewToolResultError("Error: " + err.Error()), nil
	}
	return mcp.NewToolResultText(out), nil
}

func runTool(ctx context.Context, name string, args map[string]any) (string, error) {
	switch name {
	case "jenkins_get_build":
		ref, err := coerceBuildRef(args["buildNumber"])
		if err != nil {
			return "", err
		}
		jobPath, err := resolveJobPath(ctx, argString(args, "jobPath"))
		if err != nil {
			return "", err
		}
		return jenkins.getBuildOverview(ctx, buildOverviewArgs{
			jobPath:           jobPath,
			buildNumber:       ref,
			sha:               argString(args, "sha"),
			scanLimit:         argInt(args, "scanLimit"),
			includeStages:     argBoolDefault(args, "includeStages", true),
			includeChangeSet:  argBoolDefault(args, "includeChangeSet", true),
			includeArtifacts:  argBoolDefault(args, "includeArtifacts", true),
			includeParameters: argBoolDefault(args, "includeParameters", true),
		})

	case "jenkins_get_log":
		bn, err := coerceBuildNumber(args["buildNumber"])
		if err != nil {
			return "", err
		}
		jobPath, err := resolveJobPath(ctx, argString(args, "jobPath"))
		if err != nil {
			return "", err
		}
		return jenkins.getLog(ctx, jobPath, bn, argString(args, "stageId"), argInt(args, "tailChars"), argBool(args, "full"))

	case "jenkins_get_tests":
		bn, err := coerceBuildNumber(args["buildNumber"])
		if err != nil {
			return "", err
		}
		jobPath, err := resolveJobPath(ctx, argString(args, "jobPath"))
		if err != nil {
			return "", err
		}
		return jenkins.getTestsTool(ctx, jobPath, bn, argInt(args, "maxFailures"), argBoolDefault(args, "includeStackTrace", true))

	case "jenkins_list_builds":
		jobPath, err := resolveJobPath(ctx, argString(args, "jobPath"))
		if err != nil {
			return "", err
		}
		return jenkins.listJob(ctx, jobPath, argInt(args, "limit"))

	case "jenkins_list_jobs":
		return jenkins.listJobsTool(ctx, argString(args, "folder"), argInt(args, "limit"))

	case "jenkins_get_queue":
		return jenkins.getQueueTool(ctx)

	case "jenkins_stop_build":
		bn, err := coerceBuildNumber(args["buildNumber"])
		if err != nil {
			return "", err
		}
		jobPath, err := resolveJobPath(ctx, argString(args, "jobPath"))
		if err != nil {
			return "", err
		}
		return jenkins.stopBuildTool(ctx, jobPath, bn)

	case "jenkins_trigger_build":
		jobPath, err := resolveJobPath(ctx, argString(args, "jobPath"))
		if err != nil {
			return "", err
		}
		return jenkins.triggerBuildTool(ctx, jobPath, argStringMap(args, "parameters"))

	case "jenkins_get_job_config":
		jobPath, err := resolveJobPath(ctx, argString(args, "jobPath"))
		if err != nil {
			return "", err
		}
		return jenkins.getJobConfigTool(ctx, jobPath, pickFormat(args))

	case "jenkins_update_job_config":
		patch, err := parsePatch(args["patch"])
		if err != nil {
			return "", err
		}
		jobPath, err := resolveJobPath(ctx, argString(args, "jobPath"))
		if err != nil {
			return "", err
		}
		return jenkins.updateJobConfigTool(ctx, jobPath, patch, pickFormat(args))

	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

// resolveJobPath returns the explicit jobPath, or resolves it from the current
// git remote via repoJobMap. On multiple matches it asks the client to pick one
// via MCP elicitation, falling back to an error listing the options when the
// client does not support elicitation (or the user cancels).
func resolveJobPath(ctx context.Context, arg string) (string, error) {
	if arg != "" {
		return arg, nil
	}
	matches := jenkins.resolveJobsForRemote(currentGitRemote())
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("jobPath is required (no mapping found for current git remote in repoJobMap)")
	default:
		if picked, ok := elicitJobPath(ctx, matches); ok {
			return picked, nil
		}
		return "", fmt.Errorf("multiple Jenkins jobs map to this remote (%s). Pass jobPath explicitly", strings.Join(matches, ", "))
	}
}

// elicitJobPath asks the connected client to choose one of matches via an MCP
// elicitation prompt. Returns (picked, true) only when the user accepts a valid
// choice; any error, decline, cancel, or unsupported client yields ("", false).
func elicitJobPath(ctx context.Context, matches []string) (string, bool) {
	srv := server.ServerFromContext(ctx)
	if srv == nil {
		return "", false
	}
	// Only send an elicitation if the client declared support for it. Without
	// this gate the stdio session would send the request and block forever
	// waiting on a client that never replies.
	if sess, ok := server.ClientSessionFromContext(ctx).(server.SessionWithClientInfo); !ok || sess.GetClientCapabilities().Elicitation == nil {
		return "", false
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"jobPath": map[string]any{
				"type":        "string",
				"title":       "Jenkins job",
				"description": "Which job maps to this repo?",
				"enum":        matches,
			},
		},
		"required": []string{"jobPath"},
	}
	res, err := srv.RequestElicitation(ctx, mcp.ElicitationRequest{
		Request: mcp.Request{Method: string(mcp.MethodElicitationCreate)},
		Params: mcp.ElicitationParams{
			Message:         "Multiple Jenkins jobs map to this repo. Which one?",
			RequestedSchema: schema,
		},
	})
	if err != nil || res == nil || res.Action != mcp.ElicitationResponseActionAccept {
		return "", false
	}
	content, ok := res.Content.(map[string]any)
	if !ok {
		return "", false
	}
	picked, ok := content["jobPath"].(string)
	if !ok || picked == "" {
		return "", false
	}
	// Guard against a client returning a value outside the enum.
	for _, m := range matches {
		if m == picked {
			return picked, true
		}
	}
	return "", false
}

// ── arg helpers ───────────────────────────────────────────────────────────────

// normalizeArgs accepts `job` as an alias for `jobPath`, matching the TS server.
func normalizeArgs(args map[string]any) map[string]any {
	if args == nil {
		return map[string]any{}
	}
	if _, hasJobPath := args["jobPath"].(string); !hasJobPath {
		if job, ok := args["job"].(string); ok {
			args["jobPath"] = job
		}
	}
	return args
}

func argString(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func argInt(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

func argBool(args map[string]any, key string) bool {
	b, _ := args[key].(bool)
	return b
}

func argBoolDefault(args map[string]any, key string, def bool) bool {
	if v, ok := args[key].(bool); ok {
		return v
	}
	return def
}

func argStringMap(args map[string]any, key string) map[string]string {
	m, ok := args[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		switch s := v.(type) {
		case string:
			out[k] = s
		default:
			b, _ := json.Marshal(v)
			out[k] = string(b)
		}
	}
	return out
}

// coerceBuildRef accepts a number, numeric string, or one of the lastX aliases,
// returning the value as a string ref usable in the Jenkins API path. Empty
// input yields "" (caller defaults to lastBuild).
func coerceBuildRef(value any) (string, error) {
	switch v := value.(type) {
	case nil:
		return "", nil
	case float64:
		return strconv.Itoa(int(v)), nil
	case int:
		return strconv.Itoa(v), nil
	case string:
		if v == "" {
			return "", nil
		}
		if _, err := strconv.Atoi(v); err == nil {
			return v, nil
		}
		switch v {
		case "lastBuild", "lastSuccessfulBuild", "lastFailedBuild", "lastCompletedBuild":
			return v, nil
		}
		return "", fmt.Errorf("invalid buildNumber %q. Use a number or one of: lastBuild, lastSuccessfulBuild, lastFailedBuild, lastCompletedBuild", v)
	}
	return "", fmt.Errorf("buildNumber must be a number or string")
}

// coerceBuildNumber requires an explicit numeric build number (no aliases).
func coerceBuildNumber(value any) (int, error) {
	switch v := value.(type) {
	case float64:
		return int(v), nil
	case int:
		return v, nil
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n, nil
		}
		return 0, fmt.Errorf("buildNumber must be a build number (number or numeric string). Aliases like \"lastBuild\" are not allowed for this tool — call jenkins_get_build first to resolve them")
	case nil:
		return 0, fmt.Errorf("buildNumber is required")
	}
	return 0, fmt.Errorf("buildNumber must be a number or numeric string")
}

func parsePatch(raw any) (*JobConfig, error) {
	if raw == nil {
		return nil, fmt.Errorf("patch is required")
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid patch: %w", err)
	}
	var patch JobConfig
	if err := json.Unmarshal(b, &patch); err != nil {
		return nil, fmt.Errorf("invalid patch: %w", err)
	}
	return &patch, nil
}

// buildInstructions renders the server instructions string surfaced to clients,
// mirroring the TS buildInstructions (without the live whoami round-trip, which
// the client makes lazily anyway).
func buildInstructions(config *Config) string {
	var l lines
	l.add("# jenkins-mcp")
	l.blank()
	l.add("Jenkins tooling: build status, console logs, pipeline stage breakdowns, test reports, queue inspection, and build control (trigger/stop). Prefer these tools over shelling out to `curl` or scraping the Jenkins web UI.")
	l.blank()

	if config == nil || jenkins == nil {
		l.add("## Status")
		l.add("- Jenkins is NOT configured. Set url + username + token in ~/.jenkins-mcp.json or via JENKINS_URL / JENKINS_USERNAME / JENKINS_TOKEN env vars.")
		return l.String()
	}

	l.add("## Configured")
	id, fullName := jenkins.whoami(context.Background())
	who := ""
	if id != "" {
		who = " — you are " + id
		if fullName != "" {
			who += fmt.Sprintf(" %q", fullName)
		}
	}
	l.add("- Jenkins: %s%s", config.URL, who)

	remote := currentGitRemote()
	if remote != "" {
		branch := currentGitBranch()
		sha := currentGitSha()
		l.blank()
		l.add("## Current repo")
		l.add("- Remote: %s", remote)
		if branch != "" {
			if sha != "" {
				l.add("- Branch: %s @ %s", branch, sha[:min(8, len(sha))])
			} else {
				l.add("- Branch: %s", branch)
			}
		}
		matches := jenkins.resolveJobsForRemote(remote)
		switch {
		case len(matches) > 0:
			plural := ""
			if len(matches) > 1 {
				plural = "s"
			}
			l.add("- Mapped Jenkins job%s: %s", plural, strings.Join(matches, ", "))
			if len(matches) == 1 {
				l.add("  jobPath is auto-resolved for jenkins_get_build / jenkins_get_log / jenkins_list_builds.")
			} else {
				l.add("  Multiple matches — pass jobPath explicitly.")
			}
		case jenkins.hasRepoMapping():
			l.add("- No Jenkins job mapping found for this remote. Add an entry to repoJobMap in ~/.jenkins-mcp.json.")
		default:
			l.add("- No repoJobMap configured. Pass jobPath explicitly, or add ~/.jenkins-mcp.json: { \"repoJobMap\": { \"<remote-substring>\": \"folder/job\" } }.")
		}
	}

	l.blank()
	l.add("## Use these tools")
	l.add("- \"did the build pass / status of the build\" → `jenkins_get_build` with jobPath (and optionally sha=<commit> to find the build for a specific commit, or buildNumber for an explicit build).")
	l.add("- \"why did it fail / show me the failed tests\" → `jenkins_get_tests` with jobPath + buildNumber. Returns failing test names, classes, and stack traces.")
	l.add("- \"show me the console log\" → `jenkins_get_log` with jobPath + buildNumber. Pass stageId for just one Pipeline stage, or full=true to opt in to the entire log.")
	l.add("- \"show recent builds\" → `jenkins_list_builds` with jobPath.")
	l.add("- \"what jobs exist / browse a folder\" → `jenkins_list_jobs` (pass folder for sub-folders).")
	l.add("- \"what's waiting in the queue\" → `jenkins_get_queue`.")
	l.add("- \"stop/abort a build\" → `jenkins_stop_build` with jobPath + buildNumber.")
	l.add("- \"trigger/start a build\" → `jenkins_trigger_build` with jobPath and optional parameters.")
	l.add("- \"show job config / Jenkinsfile settings\" → `jenkins_get_job_config` with jobPath.")
	l.add("- \"update job config\" → `jenkins_update_job_config` with jobPath + patch. Get current config first with jenkins_get_job_config.")
	l.blank()
	l.add("Job path syntax: nested folders use slashes, e.g. \"platform/api/build-master\". URL-encoding is handled by the tool. Tools also accept `job` as an alias for `jobPath`.")
	l.blank()
	l.add("IMPORTANT: `jenkins_trigger_build`, `jenkins_stop_build`, and `jenkins_update_job_config` change Jenkins state. Do not call them without an explicit user instruction. The read tools (build status, logs, tests, queue, job listing/config) are always safe.")
	return l.String()
}
