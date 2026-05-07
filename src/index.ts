#!/usr/bin/env node
import 'dotenv/config';
import { readFileSync } from 'fs';
import { fileURLToPath } from 'url';
import { dirname, join } from 'path';
import { Server } from '@modelcontextprotocol/sdk/server/index.js';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import {
  CallToolRequestSchema,
  ListToolsRequestSchema,
  McpError,
  ErrorCode,
} from '@modelcontextprotocol/sdk/types.js';
import { loadConfig } from './config.js';
import { JenkinsClient, type BuildRef } from './jenkins.js';
import { currentGitRemote, currentGitBranch, currentGitSha } from './git.js';

const pkg = JSON.parse(
  readFileSync(join(dirname(fileURLToPath(import.meta.url)), '../package.json'), 'utf-8')
) as { version: string };

const config = loadConfig();
const jenkins = config ? new JenkinsClient(config.url, config.username, config.token, config.repoJobMap) : null;

function normalizeArgs(args: unknown): Record<string, unknown> {
  const src = (args && typeof args === 'object') ? (args as Record<string, unknown>) : {};
  const out: Record<string, unknown> = { ...src };
  // Accept `job` as alias for `jobPath` — models often guess the shorter name
  if (typeof out.job === 'string' && typeof out.jobPath !== 'string') out.jobPath = out.job;
  return out;
}

function coerceBuildRef(value: unknown): BuildRef | undefined {
  if (value === undefined || value === null) return undefined;
  if (typeof value === 'number') return value;
  if (typeof value === 'string') {
    if (/^\d+$/.test(value)) return Number(value);
    if (value === 'lastBuild' || value === 'lastSuccessfulBuild' || value === 'lastFailedBuild' || value === 'lastCompletedBuild') {
      return value;
    }
    throw new Error(`Invalid buildNumber "${value}". Use a number or one of: lastBuild, lastSuccessfulBuild, lastFailedBuild, lastCompletedBuild.`);
  }
  throw new Error('buildNumber must be a number or string.');
}

function coerceBuildNumber(value: unknown): number {
  const ref = coerceBuildRef(value);
  if (typeof ref !== 'number') {
    throw new Error('buildNumber must be a build number (number or numeric string). Aliases like "lastBuild" are not allowed for this tool — call jenkins_get_build first to resolve them.');
  }
  return ref;
}

async function buildInstructions(): Promise<string> {
  const lines: string[] = [];
  lines.push('# jenkins-mcp');
  lines.push('');
  lines.push('Jenkins tooling: build status, console logs, pipeline stage breakdowns, test reports, queue inspection, and build control (trigger/stop). Prefer these tools over shelling out to `curl` or scraping the Jenkins web UI.');
  lines.push('');

  if (!jenkins || !config) {
    lines.push('## Status');
    lines.push('- Jenkins is NOT configured. Set url + username + token in ~/.jenkins-mcp.json or via JENKINS_URL / JENKINS_USERNAME / JENKINS_TOKEN env vars.');
    return lines.join('\n');
  }

  const me = await jenkins.whoami().catch(() => null);
  lines.push('## Configured');
  lines.push(`- Jenkins: ${config.url}${me ? ` — you are ${me.id ?? '?'}${me.fullName ? ` "${me.fullName}"` : ''}` : ''}`);

  const remote = currentGitRemote();
  const branch = currentGitBranch();
  const sha = currentGitSha();
  if (remote) {
    const matches = jenkins.resolveJobsForRemote(remote);
    lines.push('');
    lines.push('## Current repo');
    lines.push(`- Remote: ${remote}`);
    if (branch) lines.push(`- Branch: ${branch}${sha ? ` @ ${sha.slice(0, 8)}` : ''}`);
    if (matches.length > 0) {
      lines.push(`- Mapped Jenkins job${matches.length > 1 ? 's' : ''}: ${matches.join(', ')}`);
      if (matches.length === 1) {
        lines.push('  jobPath is auto-resolved for jenkins_get_build / jenkins_get_log / jenkins_list_builds.');
      } else {
        lines.push('  Multiple matches — pass jobPath explicitly, or rely on the picker prompt.');
      }
    } else if (jenkins.hasRepoMapping()) {
      lines.push('- No Jenkins job mapping found for this remote. Add an entry to repoJobMap in ~/.jenkins-mcp.json.');
    } else {
      lines.push('- No repoJobMap configured. Pass jobPath explicitly, or add ~/.jenkins-mcp.json: { "repoJobMap": { "<remote-substring>": "folder/job" } }.');
    }
  }

  lines.push('');
  lines.push('## Use these tools');
  lines.push('- "did the build pass / status of the build" → `jenkins_get_build` with jobPath (and optionally sha=<commit> to find the build for a specific commit, or buildNumber for an explicit build).');
  lines.push('- "why did it fail / show me the failed tests" → `jenkins_get_tests` with jobPath + buildNumber. Returns failing test names, classes, and stack traces.');
  lines.push('- "show me the console log" → `jenkins_get_log` with jobPath + buildNumber. Pass stageId for just one Pipeline stage, or full=true to opt in to the entire log.');
  lines.push('- "show recent builds" → `jenkins_list_builds` with jobPath.');
  lines.push('- "what jobs exist / browse a folder" → `jenkins_list_jobs` (pass folder for sub-folders).');
  lines.push('- "what\'s waiting in the queue" → `jenkins_get_queue`.');
  lines.push('- "stop/abort a build" → `jenkins_stop_build` with jobPath + buildNumber.');
  lines.push('- "trigger/start a build" → `jenkins_trigger_build` with jobPath and optional parameters.');
  lines.push('- "show job config / Jenkinsfile settings" → `jenkins_get_job_config` with jobPath. Returns raw XML config.');
  lines.push('- "update job config" → `jenkins_update_job_config` with jobPath + config (full XML). Get current config first with jenkins_get_job_config.');
  lines.push('');
  lines.push('Job path syntax: nested folders use slashes, e.g. "platform/api/build-master". URL-encoding is handled by the tool. Tools also accept `job` as an alias for `jobPath`.');

  return lines.join('\n');
}

const _instructions = await buildInstructions();

const server = new Server(
  { name: 'jenkins-mcp', version: pkg.version },
  { capabilities: { tools: {} }, instructions: _instructions }
);

server.onerror = (error) => console.error('[MCP Error]', error);

const TOOLS = [
    {
      name: 'jenkins_get_build',
      description: 'Get the status and details of a Jenkins build. Use when asked "did the build pass?", "what\'s the status of CI for this commit?", or "why did the build fail?". Pass sha to find the build matching a specific commit (scans recent builds), or buildNumber for an explicit build, or omit both to get the latest. Returns result, duration, commit, branch, cause, parameters, pipeline stage breakdown (with stage ids for jenkins_get_log), recent changes, and artifact URLs.',
      inputSchema: {
        type: 'object',
        properties: {
          jobPath:           { type: 'string', description: 'Jenkins job path. Use slashes for nested folders, e.g. "platform/api/build-master". If omitted, the server tries to resolve from the current git remote via repoJobMap (and prompts for a pick when multiple match).' },
          job:               { type: 'string', description: 'Alias for jobPath.' },
          buildNumber:       { type: ['number', 'string'], description: 'Build number (e.g. 42), or one of "lastBuild", "lastSuccessfulBuild", "lastFailedBuild", "lastCompletedBuild" (default: lastBuild)' },
          sha:               { type: 'string', description: 'Find the most recent build whose lastBuiltRevision matches this commit SHA (full or 7+ char prefix). Overrides buildNumber.' },
          scanLimit:         { type: 'number', description: 'How many recent builds to scan when sha is provided (default 30, max 200). Increase for repos with many builds per commit.', default: 30 },
          includeStages:     { type: 'boolean', description: 'Include Pipeline stage breakdown when available (default true)', default: true },
          includeChangeSet:  { type: 'boolean', description: 'Include the commits in this build (default true)', default: true },
          includeArtifacts:  { type: 'boolean', description: 'Include artifact URLs (default true)', default: true },
          includeParameters: { type: 'boolean', description: 'Include build parameters (default true)', default: true },
        },
      },
    },
    {
      name: 'jenkins_get_log',
      description: 'Fetch the console log for a Jenkins build. Use when diagnosing a failure after jenkins_get_build shows result=FAILURE/UNSTABLE. By default returns the tail (last 200KB) to stay within context — pass full=true for everything, or tailChars to set a different cap. For Pipeline jobs, pass stageId (from jenkins_get_build output) to get just that stage\'s logs, which is dramatically smaller and more focused than the whole console.',
      inputSchema: {
        type: 'object',
        properties: {
          jobPath:     { type: 'string', description: 'Jenkins job path (auto-resolved from current repo when omitted).' },
          job:         { type: 'string', description: 'Alias for jobPath.' },
          buildNumber: { type: ['number', 'string'], description: 'Build number to fetch the log for (numeric).' },
          stageId:     { type: 'string', description: 'Optional Pipeline stage id (from jenkins_get_build). Returns just that stage\'s per-step logs.' },
          tailChars:   { type: 'number', description: 'Return only the last N characters (default 200000). Ignored if full=true.', default: 200000 },
          full:        { type: 'boolean', description: 'Return the entire log without truncation. Use sparingly — large jobs can produce megabytes.', default: false },
        },
        required: ['buildNumber'],
      },
    },
    {
      name: 'jenkins_get_tests',
      description: 'Fetch the test report for a build. Use when asked "which tests failed?" or "why did this build fail?" — surfaces failed test names, classes, and stack traces directly, which is far more useful than grepping the console log. Returns 404 gracefully when the job does not publish JUnit/xUnit results.',
      inputSchema: {
        type: 'object',
        properties: {
          jobPath:           { type: 'string', description: 'Jenkins job path (auto-resolved from current repo when omitted).' },
          job:               { type: 'string', description: 'Alias for jobPath.' },
          buildNumber:       { type: ['number', 'string'], description: 'Build number (numeric).' },
          maxFailures:       { type: 'number', description: 'Max failed cases to print (default 25)', default: 25 },
          includeStackTrace: { type: 'boolean', description: 'Include stack trace excerpts for each failure (default true)', default: true },
        },
        required: ['buildNumber'],
      },
    },
    {
      name: 'jenkins_list_builds',
      description: 'List recent builds for a Jenkins job. Use when asked "show me the last few builds", "is master passing?", or to find a build number to drill into. Each row shows result, duration, timestamp, and the built commit.',
      inputSchema: {
        type: 'object',
        properties: {
          jobPath: { type: 'string', description: 'Jenkins job path (auto-resolved from current repo when omitted).' },
          job:     { type: 'string', description: 'Alias for jobPath.' },
          limit:   { type: 'number', description: 'Max builds to return (default 20, max 200)', default: 20 },
        },
      },
    },
    {
      name: 'jenkins_list_jobs',
      description: 'List jobs in Jenkins, either at the root or inside a folder. Use to discover the right jobPath when none is mapped, or to browse a multibranch folder. Folder entries are tagged [folder] — pass them back as the folder arg to descend.',
      inputSchema: {
        type: 'object',
        properties: {
          folder: { type: 'string', description: 'Folder path to list (e.g. "platform/api"). Omit for top-level jobs.' },
          limit:  { type: 'number', description: 'Max entries (default 50, max 200)', default: 50 },
        },
      },
    },
    {
      name: 'jenkins_get_queue',
      description: 'Show items currently waiting in the Jenkins build queue. Use when asked "is anything waiting to build?", "why hasn\'t the build started?", or to see what would run if executors freed up.',
      inputSchema: {
        type: 'object',
        properties: {},
      },
    },
    {
      name: 'jenkins_stop_build',
      description: 'Abort a running Jenkins build. Use when asked to "stop", "cancel", or "abort" a build. Sends a stop signal — the build may take a few seconds to terminate. Has no effect on builds that are already complete.',
      inputSchema: {
        type: 'object',
        properties: {
          jobPath:     { type: 'string', description: 'Jenkins job path.' },
          job:         { type: 'string', description: 'Alias for jobPath.' },
          buildNumber: { type: ['number', 'string'], description: 'Build number to stop (numeric).' },
        },
        required: ['buildNumber'],
      },
    },
    {
      name: 'jenkins_trigger_build',
      description: 'Trigger a new Jenkins build, optionally with parameters. Use when asked to "run", "start", "kick off", or "trigger" a build. Returns a queue item URL — use jenkins_get_queue to monitor it, or jenkins_list_builds once it starts.',
      inputSchema: {
        type: 'object',
        properties: {
          jobPath:    { type: 'string', description: 'Jenkins job path.' },
          job:        { type: 'string', description: 'Alias for jobPath.' },
          parameters: {
            type: 'object',
            description: 'Optional key/value build parameters (for parameterized jobs). Must match the parameter names defined on the job.',
            additionalProperties: { type: 'string' },
          },
        },
      },
    },
    {
      name: 'jenkins_get_job_config',
      description: 'Fetch the raw XML configuration for a Jenkins job. Use when asked to "show the job config", "what does the Jenkinsfile point to?", or before making changes with jenkins_update_job_config. Pipeline jobs include the script path or inline Groovy. Freestyle jobs include build steps and triggers.',
      inputSchema: {
        type: 'object',
        properties: {
          jobPath: { type: 'string', description: 'Jenkins job path.' },
          job:     { type: 'string', description: 'Alias for jobPath.' },
        },
      },
    },
    {
      name: 'jenkins_update_job_config',
      description: 'Replace the XML configuration for a Jenkins job. Always call jenkins_get_job_config first, modify the XML, then pass the full updated XML here. Overwrites the entire job config — partial updates are not supported. Use to change branch specs, pipeline script paths, build triggers, parameters, or other job settings.',
      inputSchema: {
        type: 'object',
        properties: {
          jobPath: { type: 'string', description: 'Jenkins job path.' },
          job:     { type: 'string', description: 'Alias for jobPath.' },
          config:  { type: 'string', description: 'Full Jenkins job XML config. Must be the complete document — get the current config with jenkins_get_job_config, modify, then pass here.' },
        },
        required: ['config'],
      },
    },
];

server.setRequestHandler(ListToolsRequestSchema, async () => ({ tools: TOOLS }));

async function resolveJobPath(arg: string | undefined): Promise<string> {
  if (arg) return arg;
  if (!jenkins) throw new Error('Jenkins not configured');
  const remote = currentGitRemote();
  const matches = jenkins.resolveJobsForRemote(remote);
  if (matches.length === 1) return matches[0];
  if (matches.length === 0) {
    throw new Error('jobPath is required (no mapping found for current git remote in repoJobMap).');
  }

  // Multiple matches — try elicitInput, fall back to listing options
  try {
    const pickerResult = await server.elicitInput({
      message: `Multiple Jenkins jobs map to this remote. Which one?`,
      requestedSchema: {
        type: 'object',
        properties: {
          jobPath: {
            type: 'string',
            title: 'Select a job',
            oneOf: [
              ...matches.map((m) => ({ const: m, title: m })),
              { const: '__cancel__', title: 'Cancel' },
            ],
          },
        },
        required: ['jobPath'],
      },
    });
    if (
      pickerResult.action === 'cancel' ||
      pickerResult.action === 'decline' ||
      pickerResult.content?.jobPath === '__cancel__'
    ) {
      throw new Error('Cancelled.');
    }
    const picked = pickerResult.content?.jobPath as string | undefined;
    if (!picked || picked === '__cancel__') throw new Error('Cancelled.');
    return picked;
  } catch (err) {
    // elicitInput not supported by client — surface the options
    if (err instanceof Error && err.message === 'Cancelled.') throw err;
    throw new Error(
      `Multiple Jenkins jobs map to this remote (${matches.join(', ')}). Pass jobPath explicitly.`,
    );
  }
}

server.setRequestHandler(CallToolRequestSchema, async (request) => {
  const { name, arguments: rawArgs = {} } = request.params;
  if (!jenkins) {
    return {
      content: [{ type: 'text', text: 'Jenkins is not configured. Set url + username + token in ~/.jenkins-mcp.json or via JENKINS_URL / JENKINS_USERNAME / JENKINS_TOKEN env vars.' }],
      isError: true,
    };
  }
  const args = normalizeArgs(rawArgs);
  try {
    switch (name) {
      case 'jenkins_get_build': {
        const a = args as {
          jobPath?: string; buildNumber?: unknown; sha?: string;
          scanLimit?: number; includeStages?: boolean; includeChangeSet?: boolean;
          includeArtifacts?: boolean; includeParameters?: boolean;
        };
        return await jenkins.getBuildOverview({
          jobPath: await resolveJobPath(a.jobPath),
          buildNumber: coerceBuildRef(a.buildNumber),
          sha: a.sha,
          scanLimit: a.scanLimit,
          includeStages: a.includeStages,
          includeChangeSet: a.includeChangeSet,
          includeArtifacts: a.includeArtifacts,
          includeParameters: a.includeParameters,
        });
      }
      case 'jenkins_get_log': {
        const a = args as { jobPath?: string; buildNumber: unknown; stageId?: string; tailChars?: number; full?: boolean };
        return await jenkins.getLog({
          jobPath: await resolveJobPath(a.jobPath),
          buildNumber: coerceBuildNumber(a.buildNumber),
          stageId: a.stageId,
          tailChars: a.tailChars,
          full: a.full,
        });
      }
      case 'jenkins_get_tests': {
        const a = args as { jobPath?: string; buildNumber: unknown; maxFailures?: number; includeStackTrace?: boolean };
        return await jenkins.getTestsTool({
          jobPath: await resolveJobPath(a.jobPath),
          buildNumber: coerceBuildNumber(a.buildNumber),
          maxFailures: a.maxFailures,
          includeStackTrace: a.includeStackTrace,
        });
      }
      case 'jenkins_list_builds': {
        const a = args as { jobPath?: string; limit?: number };
        return await jenkins.listJob({ jobPath: await resolveJobPath(a.jobPath), limit: a.limit });
      }
      case 'jenkins_list_jobs': {
        const a = args as { folder?: string; limit?: number };
        return await jenkins.listJobsTool(a);
      }
      case 'jenkins_get_queue':
        return await jenkins.getQueueTool();
      case 'jenkins_stop_build': {
        const a = args as { jobPath?: string; buildNumber: unknown };
        return await jenkins.stopBuildTool({
          jobPath: await resolveJobPath(a.jobPath),
          buildNumber: coerceBuildNumber(a.buildNumber),
        });
      }
      case 'jenkins_trigger_build': {
        const a = args as { jobPath?: string; parameters?: Record<string, string> };
        return await jenkins.triggerBuildTool({
          jobPath: await resolveJobPath(a.jobPath),
          parameters: a.parameters,
        });
      }
      case 'jenkins_get_job_config': {
        const a = args as { jobPath?: string };
        return await jenkins.getJobConfigTool({ jobPath: await resolveJobPath(a.jobPath) });
      }
      case 'jenkins_update_job_config': {
        const a = args as { jobPath?: string; config: string };
        if (!a.config) throw new Error('config is required');
        return await jenkins.updateJobConfigTool({
          jobPath: await resolveJobPath(a.jobPath),
          config: a.config,
        });
      }
      default:
        throw new McpError(ErrorCode.MethodNotFound, `Unknown tool: ${name}`);
    }
  } catch (err) {
    if (err instanceof McpError) throw err;
    return {
      content: [{ type: 'text', text: `Error: ${(err as Error).message}` }],
      isError: true,
    };
  }
});

async function shutdown() {
  await server.close();
  process.exit(0);
}
process.on('SIGINT', shutdown);
process.on('SIGTERM', shutdown);

const transport = new StdioServerTransport();
await server.connect(transport);
