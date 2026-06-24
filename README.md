# jenkins-mcp

A [Model Context Protocol](https://modelcontextprotocol.io) (MCP) server for **Jenkins**, written in Go. Inspect builds, control jobs, and manage pipeline configuration — designed to pair with [`atlassian-mcp`](https://github.com/stubbedev/atlassian-mcp) so you can ask "did the build pass?" alongside Jira and Bitbucket questions.

Distributed three ways — all from the same source: a zero-Node **prebuilt binary** (`go install` or a release download), an **npm wrapper** (`npx @stubbedev/jenkins-mcp`) that fetches the matching binary so every config below works unchanged, and a **Nix flake** (`nix run github:stubbedev/jenkins-mcp`).

---

## Tools

### Inspection

| Tool | Description |
|---|---|
| `jenkins_get_build` | Status of a build by number, by SHA (auto-finds the build for a commit), or the latest. Includes Pipeline stage breakdown, parameters, changeset, and artifact URLs. |
| `jenkins_get_log` | Console log for a build. Pass `stageId` for just one Pipeline stage; defaults to a 200KB tail (use `full=true` to opt in to the entire log). |
| `jenkins_get_tests` | Test report — failing test names, classes, and stack traces. Far more useful than grepping the console log when a build fails. |
| `jenkins_list_builds` | Recent builds for a job, with result + duration + commit. |
| `jenkins_list_jobs` | Browse jobs at the root or inside a folder. |
| `jenkins_get_queue` | Items currently waiting in the build queue. |

### Build control

| Tool | Description |
|---|---|
| `jenkins_trigger_build` | Trigger a new build, optionally with parameters. Returns a queue item URL to track progress. |
| `jenkins_stop_build` | Abort a running build by number. |

### Job configuration

| Tool | Description |
|---|---|
| `jenkins_get_job_config` | Fetch a job's configuration as a structured object (TOON by default) — pipeline definition, triggers, parameters, and retention settings. |
| `jenkins_update_job_config` | Patch a job's configuration. Pass only the fields to change; everything else is preserved. Supports description, pipeline script/path, triggers, parameters, and build retention. |

### Natural-language examples

- "did CI pass for this commit?" → `jenkins_get_build` with `sha=<HEAD>`
- "why did build #42 fail?" → `jenkins_get_tests` (failing tests + stack traces) or `jenkins_get_log` with `stageId=<id>` for the failing Pipeline stage
- "show me the last 10 builds of master" → `jenkins_list_builds`
- "what jobs exist under platform/api?" → `jenkins_list_jobs` with `folder=platform/api`
- "what's waiting in the queue?" → `jenkins_get_queue`
- "trigger a build with ENV=production" → `jenkins_trigger_build` with `parameters`
- "stop the current build" → `jenkins_stop_build` with `buildNumber`
- "add a nightly cron trigger to this job" → `jenkins_get_job_config` then `jenkins_update_job_config` with updated `triggers`

---

## Setup

### 1. Generate a Jenkins API token

1. In Jenkins, click your username (top-right) → **Configure**.
2. In the **API Token** section, click **Add new token**, give it a name, and click **Generate**.
3. Copy the token value — it is only shown once.

### 2. Create a config file

Create `~/.jenkins-mcp.json`:

```json
{
  "$schema": "https://raw.githubusercontent.com/stubbedev/jenkins-mcp/master/jenkins-mcp.schema.json",
  "url": "https://jenkins.example.com",
  "username": "your-jenkins-login",
  "token": "your-api-token",
  "repoJobMap": {
    "stubbedev/atlassian-mcp": "atlassian-mcp/master",
    "kontainer/api": ["kontainer-api/master", "kontainer-api/PR-builds"]
  }
}
```

The `$schema` field is optional but enables editor autocomplete and validation.

`repoJobMap` keys are matched as case-insensitive substrings against the git remote URL of the current repo, so the server can resolve `jobPath` automatically when you call `jenkins_get_build` from inside a repo. Values can be a single job path or an array. Job paths use slashes for nested folders (e.g. `folder/sub-folder/job-name`).

Alternatively, use environment variables (or a `.env` file in this directory):

```env
JENKINS_URL=https://jenkins.example.com
JENKINS_USERNAME=your-jenkins-login
JENKINS_TOKEN=your-api-token
```

Config is resolved in this order: `--config <path>` CLI arg → `JENKINS_MCP_CONFIG` env var → `~/.jenkins-mcp.json` → `$XDG_CONFIG_HOME/jenkins-mcp/config.json` (default `~/.config/jenkins-mcp/config.json`) → `.jenkins-mcp.json` in cwd → environment variables.

### 3. Connect to your AI tool

No cloning or building required — point your tool at `npx @stubbedev/jenkins-mcp@latest` and it will install and run automatically.

---

#### Claude Code

```bash
claude mcp add jenkins -- npx -y @stubbedev/jenkins-mcp@latest --config ~/.jenkins-mcp.json
```

---

#### Cursor

Add to `~/.cursor/mcp.json` (global) or `.cursor/mcp.json` (project-only):

```json
{
  "mcpServers": {
    "jenkins": {
      "command": "npx",
      "args": ["-y", "@stubbedev/jenkins-mcp@latest", "--config", "/Users/you/.jenkins-mcp.json"]
    }
  }
}
```

---

#### Windsurf

Add to `~/.codeium/windsurf/mcp_config.json`:

```json
{
  "mcpServers": {
    "jenkins": {
      "command": "npx",
      "args": ["-y", "@stubbedev/jenkins-mcp@latest", "--config", "/Users/you/.jenkins-mcp.json"]
    }
  }
}
```

---

#### Zed

Add to `~/.config/zed/settings.json`:

```json
{
  "context_servers": {
    "jenkins": {
      "command": {
        "path": "npx",
        "args": ["-y", "@stubbedev/jenkins-mcp@latest", "--config", "/home/you/.jenkins-mcp.json"]
      }
    }
  }
}
```

---

#### OpenCode

Add to `opencode.json` in your project root (or `~/.config/opencode/opencode.json` for global):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "jenkins": {
      "type": "local",
      "command": ["npx", "-y", "@stubbedev/jenkins-mcp@latest", "--config", "/home/you/.jenkins-mcp.json"]
    }
  }
}
```

---

#### Codex CLI

Add to `~/.codex/config.yaml`:

```yaml
mcpServers:
  jenkins:
    command: npx
    args:
      - -y
      - @stubbedev/jenkins-mcp@latest
      - --config
      - /home/you/.jenkins-mcp.json
```

---

#### Go install (no Node)

If you have Go installed and prefer a native binary on your `PATH`:

```bash
go install github.com/stubbedev/jenkins-mcp@latest
```

Then point your MCP client at the `jenkins-mcp` command directly, e.g. for Claude Code:

```bash
claude mcp add jenkins -- jenkins-mcp --config ~/.jenkins-mcp.json
```

Prebuilt binaries for every platform are also attached to each [GitHub release](https://github.com/stubbedev/jenkins-mcp/releases) if you'd rather not build.

---

#### Nix (NixOS / nix-darwin / home-manager)

Run it straight from the flake — no clone, no install:

```bash
nix run github:stubbedev/jenkins-mcp -- --config ~/.jenkins-mcp.json
```

Or reference it in your MCP client config with that command, or add the flake as an input and use `packages.default` in your system/home configuration. The flake builds from source with `buildGoModule`; its version tracks `package.json` automatically and CI keeps the `vendorHash` current.

---

#### Any other MCP-compatible tool

Most tools that support MCP accept the same JSON format. Use `npx` as the command with `["-y", "@stubbedev/jenkins-mcp@latest", "--config", "/path/to/config.json"]` as the args — or the `jenkins-mcp` binary directly.

### Updating existing installs

If your MCP client is already configured and you want the newest package version:

```bash
npx clear-npx-cache
```

Then restart your MCP client.

---

### Manual install (optional)

If you prefer to clone and build locally (requires Go 1.26+):

```bash
git clone git@github.com:stubbedev/jenkins-mcp.git
cd jenkins-mcp
go build -o jenkins-mcp .
```

Then use `/path/to/jenkins-mcp/jenkins-mcp` instead of the `npx` command in the configs above.

---

## Output format (TOON)

Structured tool responses (`jenkins_get_job_config`, and the config echoed back by `jenkins_update_job_config`) are serialized as [TOON](https://github.com/toon-format/toon) (Token-Oriented Object Notation) by default — a compact, LLM-friendly format that uses fewer tokens than equivalent JSON. Pass `format: "json"` on those tools to get JSON instead, or set `JENKINS_MCP_FORMAT=json` to flip the default process-wide. The build/log/test/queue tools already return purpose-built plain text and are unaffected.

---

## Notes

- Authentication is HTTP Basic with `username:apitoken` (Jenkins's standard mechanism).
- Write operations (`jenkins_trigger_build`, `jenkins_stop_build`, `jenkins_update_job_config`) require the API token user to have the appropriate Jenkins permissions.
- CSRF crumb handling is automatic — API tokens bypass the crumb check on most Jenkins installations, but the server fetches and caches a crumb for installations that require it.
- `jenkins_get_build` with `sha` scans the last 30 builds of the job for a matching `lastBuiltRevision`. Increase `scanLimit` for repos with many builds per commit.
- Pipeline stage breakdown comes from Jenkins's `wfapi` (workflow plugin). Freestyle jobs don't expose it, and the tool falls back to summary fields only.
- Test reports come from the standard Jenkins JUnit / xUnit plugins. Jobs that don't publish results return a friendly 404 message.

---

## Releases (Maintainers)

Each release ships **both** prebuilt Go binaries (attached to the GitHub release) and the npm wrapper `@stubbedev/jenkins-mcp`. `.github/workflows/publish.yml` runs on a pushed `v*` tag and: cross-compiles binaries for 14 targets — linux (amd64, arm64, arm/v7, 386, ppc64le, s390x, riscv64), darwin (amd64, arm64), windows (amd64, arm64, 386), and freebsd (amd64, arm64) — attaches them to the GitHub release, then publishes the npm package. The Nix flake builds from the tagged source and tracks `package.json` for its version, so it needs no separate release step.

Use semantic versioning. Breaking tool-surface changes should bump the minor version while `<1.0.0` (for example `0.0.x` -> `0.1.0`).

Release flow (the `preversion` hook runs `go vet`, `go test`, and the smoke check first):

```bash
# choose one: patch | minor | major
npm run release:minor
```

This bumps `package.json`, commits, tags, and pushes; the pushed tag drives the publish workflow.

- The npm step uses npm Trusted Publisher (OIDC), so no `NPM_TOKEN` secret is required
- The release step uses the default `GITHUB_TOKEN`

Required npm setup (one-time):

- In npm package settings, add this GitHub repo/workflow as a Trusted Publisher

---

## Development

Requires Go 1.26+. Dependencies: [`mark3labs/mcp-go`](https://github.com/mark3labs/mcp-go) (MCP server framework), [`toon-format/toon-go`](https://github.com/toon-format/toon-go) (TOON output), and [`beevik/etree`](https://github.com/beevik/etree) (lossless config.xml editing).

```bash
# Build
go build -o jenkins-mcp .

# Run directly
./jenkins-mcp --config /path/to/config.json

# Vet + test
go vet ./...
go test ./...

# Quick smoke check (builds + validates tools/list)
npm run smoke

# Test the tool list by hand
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' | ./jenkins-mcp
```

Tool schemas live in `tools.json` (embedded into the binary via `go:embed`). The npm wrapper lives in `bin/cli.mjs` + `scripts/`. The Nix flake is `flake.nix`.
