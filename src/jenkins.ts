type ToolResult = { content: Array<{ type: 'text'; text: string }> };

export type BuildRef = number | 'lastBuild' | 'lastSuccessfulBuild' | 'lastFailedBuild' | 'lastCompletedBuild';

interface BuildSummary {
  number: number;
  url: string;
  result: BuildResult;
  building: boolean;
  timestamp: number;
  duration: number;
  estimatedDuration?: number;
  displayName?: string;
  fullDisplayName?: string;
  description?: string;
  actions?: BuildAction[];
  artifacts?: BuildArtifact[];
  changeSet?: { items?: ChangeSetItem[] };
  changeSets?: Array<{ items?: ChangeSetItem[] }>;
}

type BuildResult = 'SUCCESS' | 'FAILURE' | 'UNSTABLE' | 'ABORTED' | 'NOT_BUILT' | null;

interface BuildAction {
  _class?: string;
  lastBuiltRevision?: { SHA1?: string; branch?: Array<{ SHA1?: string; name?: string }> };
  buildsByBranchName?: Record<string, { revision?: { SHA1?: string } }>;
  causes?: Array<{ shortDescription?: string; userId?: string; userName?: string; upstreamProject?: string; upstreamBuild?: number }>;
  parameters?: Array<{ name?: string; value?: unknown }>;
}

interface BuildArtifact {
  fileName: string;
  relativePath: string;
  displayPath?: string;
}

interface ChangeSetItem {
  commitId?: string;
  author?: { fullName?: string };
  msg?: string;
  date?: string;
}

interface JobInfo {
  _class?: string;
  name: string;
  fullName?: string;
  url: string;
  buildable?: boolean;
  color?: string;
  description?: string;
  lastBuild?: { number: number; url: string };
  lastSuccessfulBuild?: { number: number; url: string };
  lastFailedBuild?: { number: number; url: string };
  lastCompletedBuild?: { number: number; url: string };
  inQueue?: boolean;
  builds?: Array<{ number: number; url: string }>;
}

interface QueueItem {
  id: number;
  url: string;
  why?: string;
  inQueueSince?: number;
  task?: { name?: string; url?: string };
}

interface PipelineDescribe {
  id: string;
  name: string;
  status: string;
  startTimeMillis: number;
  endTimeMillis?: number;
  durationMillis?: number;
  stages: PipelineStage[];
}

interface PipelineStage {
  id: string;
  name: string;
  status: string;
  startTimeMillis: number;
  durationMillis: number;
  error?: { message?: string; type?: string };
  _links?: { self?: { href: string }; log?: { href: string } };
}

interface PipelineStageDetails {
  id: string;
  name: string;
  status: string;
  startTimeMillis: number;
  durationMillis: number;
  stageFlowNodes?: Array<{
    id: string;
    name: string;
    status: string;
    error?: { message?: string };
    _links?: { log?: { href: string } };
  }>;
}

interface PipelineLogPayload {
  nodeId?: string;
  nodeStatus?: string;
  length?: number;
  hasMore?: boolean;
  text?: string;
}

interface TestReport {
  failCount?: number;
  skipCount?: number;
  passCount?: number;
  duration?: number;
  suites?: TestSuite[];
}

interface TestSuite {
  name?: string;
  duration?: number;
  cases?: TestCase[];
}

interface TestCase {
  className?: string;
  name?: string;
  status?: 'PASSED' | 'FAILED' | 'SKIPPED' | 'FIXED' | 'REGRESSION';
  duration?: number;
  errorDetails?: string;
  errorStackTrace?: string;
}

const KNOWN_FOLDER_CLASSES = new Set([
  'com.cloudbees.hudson.plugins.folder.Folder',
  'org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject',
  'jenkins.branch.OrganizationFolder',
]);

const DEFAULT_LOG_TAIL_CHARS = 200_000;
const REQUEST_TIMEOUT_MS = 30_000;
const LOG_REQUEST_TIMEOUT_MS = 90_000;

function text(t: string): ToolResult {
  return { content: [{ type: 'text', text: t }] };
}

function fmtDuration(ms: number): string {
  if (!ms || ms < 0) return '?';
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  const rs = s % 60;
  if (m < 60) return rs ? `${m}m${rs}s` : `${m}m`;
  const h = Math.floor(m / 60);
  const rm = m % 60;
  return rm ? `${h}h${rm}m` : `${h}h`;
}

function fmtTimestamp(ms: number): string {
  if (!ms) return '?';
  return new Date(ms).toISOString().replace('T', ' ').replace(/\.\d{3}Z$/, 'Z');
}

function fmtResult(r: BuildResult, building: boolean): string {
  if (building) return 'BUILDING';
  return r ?? 'UNKNOWN';
}

function jobPathToApiSegments(jobPath: string): string {
  return jobPath
    .split('/')
    .filter(Boolean)
    .map((seg) => `job/${encodeURIComponent(seg)}`)
    .join('/');
}

function extractCommitFromActions(actions?: BuildAction[]): string | undefined {
  if (!actions) return undefined;
  for (const a of actions) {
    if (a.lastBuiltRevision?.SHA1) return a.lastBuiltRevision.SHA1;
    if (a.buildsByBranchName) {
      for (const v of Object.values(a.buildsByBranchName)) {
        if (v.revision?.SHA1) return v.revision.SHA1;
      }
    }
  }
  return undefined;
}

function extractBranchFromActions(actions?: BuildAction[]): string | undefined {
  if (!actions) return undefined;
  for (const a of actions) {
    const branchName = a.lastBuiltRevision?.branch?.[0]?.name;
    if (branchName) return branchName;
  }
  return undefined;
}

function extractCauseSummary(actions?: BuildAction[]): string | undefined {
  if (!actions) return undefined;
  for (const a of actions) {
    if (a.causes && a.causes.length > 0) {
      return a.causes
        .map((c) => c.shortDescription)
        .filter(Boolean)
        .join('; ');
    }
  }
  return undefined;
}

function extractParameters(actions?: BuildAction[]): Array<{ name: string; value: string }> {
  if (!actions) return [];
  const out: Array<{ name: string; value: string }> = [];
  for (const a of actions) {
    if (!a.parameters) continue;
    for (const p of a.parameters) {
      if (!p.name) continue;
      const v = p.value;
      const value = v === null || v === undefined ? '' : typeof v === 'string' ? v : JSON.stringify(v);
      out.push({ name: p.name, value });
    }
  }
  return out;
}

function shaPrefixMatches(target: string, candidate: string): boolean {
  const t = target.toLowerCase();
  const c = candidate.toLowerCase();
  const n = Math.min(t.length, c.length);
  if (n < 7) return false;
  return t.slice(0, n) === c.slice(0, n);
}

function parseJenkinsErrorBody(body: string): string {
  const trimmed = body.trim();
  if (!trimmed) return '';
  // Jenkins often returns HTML error pages; pull out a useful line if we can
  const titleMatch = trimmed.match(/<title>([^<]+)<\/title>/i);
  if (titleMatch) return titleMatch[1].trim();
  const h1Match = trimmed.match(/<h1>([^<]+)<\/h1>/i);
  if (h1Match) return h1Match[1].trim();
  return trimmed.length > 400 ? `${trimmed.slice(0, 400)}...` : trimmed;
}

function formatJenkinsError(status: number, method: string, path: string, body: string): string {
  const details = parseJenkinsErrorBody(body);
  const prefix = `Jenkins ${status} ${method} ${path}`;
  if (status === 401) return `${prefix}. Authentication failed. Check Jenkins username + API token (generate at <jenkins>/me/configure).`;
  if (status === 403) return `${prefix}. Permission denied. The user lacks access to this job/build, or a CSRF crumb is required (api tokens normally bypass this).`;
  if (status === 404) return `${prefix}. Not found. Verify the jobPath uses correct nesting (e.g. "folder/sub/job") and the build number exists.`;
  if (status === 500 || status === 502 || status === 503) return `${prefix}. Jenkins is unavailable or returned an error. ${details}`.trim();
  return details ? `${prefix}. ${details}` : prefix;
}

export class JenkinsClient {
  private baseUrl: string;
  private headers: Record<string, string>;
  private repoJobMap: Record<string, string[]>;
  private currentUserCache?: { id?: string; fullName?: string };

  constructor(baseUrl: string, username: string, token: string, repoJobMap?: Record<string, string | string[]>) {
    this.baseUrl = baseUrl.replace(/\/$/, '');
    const auth = Buffer.from(`${username}:${token}`).toString('base64');
    this.headers = {
      Authorization: `Basic ${auth}`,
      Accept: 'application/json',
    };
    const map: Record<string, string[]> = {};
    for (const [k, v] of Object.entries(repoJobMap ?? {})) {
      map[k.toLowerCase()] = Array.isArray(v) ? v : [v];
    }
    this.repoJobMap = map;
  }

  private async request<T>(method: string, path: string, opts: { accept?: string; raw?: boolean; timeoutMs?: number } = {}): Promise<T> {
    const url = path.startsWith('http') ? path : `${this.baseUrl}${path}`;
    const headers: Record<string, string> = { ...this.headers };
    if (opts.accept) headers.Accept = opts.accept;
    const res = await fetch(url, {
      method,
      headers,
      signal: AbortSignal.timeout(opts.timeoutMs ?? REQUEST_TIMEOUT_MS),
    });
    if (!res.ok) {
      const errText = await res.text();
      throw new Error(formatJenkinsError(res.status, method, path, errText));
    }
    if (opts.raw) return (await res.text()) as unknown as T;
    return (await res.json()) as T;
  }

  async whoami(): Promise<{ id?: string; fullName?: string } | null> {
    if (this.currentUserCache) return this.currentUserCache;
    try {
      const me = await this.request<{ id?: string; fullName?: string }>('GET', '/me/api/json');
      this.currentUserCache = me;
      return me;
    } catch {
      return null;
    }
  }

  resolveJobsForRemote(remote: string): string[] {
    if (!remote) return [];
    const lc = remote.toLowerCase();
    const matches: string[] = [];
    for (const [needle, jobs] of Object.entries(this.repoJobMap)) {
      if (lc.includes(needle)) matches.push(...jobs);
    }
    return [...new Set(matches)];
  }

  hasRepoMapping(): boolean {
    return Object.keys(this.repoJobMap).length > 0;
  }

  async getJob(jobPath: string): Promise<JobInfo> {
    const segs = jobPathToApiSegments(jobPath);
    return await this.request<JobInfo>('GET', `/${segs}/api/json?tree=name,fullName,url,buildable,color,description,inQueue,lastBuild[number,url],lastSuccessfulBuild[number,url],lastFailedBuild[number,url],lastCompletedBuild[number,url]`);
  }

  async getBuild(jobPath: string, buildNumber: BuildRef): Promise<BuildSummary> {
    const segs = jobPathToApiSegments(jobPath);
    const tree = 'number,url,result,building,timestamp,duration,estimatedDuration,displayName,fullDisplayName,description,artifacts[fileName,relativePath,displayPath],actions[lastBuiltRevision[SHA1,branch[SHA1,name]],buildsByBranchName[*[revision[SHA1]]],causes[shortDescription,userId,userName,upstreamProject,upstreamBuild],parameters[name,value]],changeSets[items[commitId,author[fullName],msg,date]]';
    return await this.request<BuildSummary>('GET', `/${segs}/${buildNumber}/api/json?tree=${encodeURIComponent(tree)}`);
  }

  async listBuilds(jobPath: string, limit: number): Promise<BuildSummary[]> {
    const segs = jobPathToApiSegments(jobPath);
    const range = Math.max(1, Math.min(200, limit));
    const tree = `builds[number,url,result,building,timestamp,duration,displayName,actions[lastBuiltRevision[SHA1,branch[name]],causes[shortDescription]]]{0,${range}}`;
    const res = await this.request<{ builds?: BuildSummary[] }>('GET', `/${segs}/api/json?tree=${encodeURIComponent(tree)}`);
    return res.builds ?? [];
  }

  async findBuildBySha(jobPath: string, sha: string, scanLimit = 30): Promise<BuildSummary | null> {
    const builds = await this.listBuilds(jobPath, scanLimit);
    for (const b of builds) {
      const candidate = extractCommitFromActions(b.actions);
      if (candidate && shaPrefixMatches(sha, candidate)) return b;
    }
    return null;
  }

  async getConsoleLog(jobPath: string, buildNumber: number, opts: { tailChars?: number } = {}): Promise<string> {
    const segs = jobPathToApiSegments(jobPath);
    const body = await this.request<string>('GET', `/${segs}/${buildNumber}/consoleText`, {
      accept: 'text/plain',
      raw: true,
      timeoutMs: LOG_REQUEST_TIMEOUT_MS,
    });
    if (opts.tailChars && body.length > opts.tailChars) {
      return `... (truncated, last ${opts.tailChars} of ${body.length} chars)\n` + body.slice(-opts.tailChars);
    }
    return body;
  }

  async getPipelineStages(jobPath: string, buildNumber: number): Promise<PipelineDescribe | null> {
    const segs = jobPathToApiSegments(jobPath);
    try {
      return await this.request<PipelineDescribe>('GET', `/${segs}/${buildNumber}/wfapi/describe`);
    } catch (err) {
      const msg = (err as Error).message;
      if (msg.includes(' 404 ')) return null;
      throw err;
    }
  }

  async getPipelineStageDetails(jobPath: string, buildNumber: number, stageId: string): Promise<PipelineStageDetails | null> {
    const segs = jobPathToApiSegments(jobPath);
    try {
      return await this.request<PipelineStageDetails>('GET', `/${segs}/${buildNumber}/execution/node/${encodeURIComponent(stageId)}/wfapi/describe`);
    } catch (err) {
      const msg = (err as Error).message;
      if (msg.includes(' 404 ')) return null;
      throw err;
    }
  }

  async getPipelineNodeLog(jobPath: string, buildNumber: number, nodeId: string): Promise<string | null> {
    const segs = jobPathToApiSegments(jobPath);
    try {
      const payload = await this.request<PipelineLogPayload>(
        'GET',
        `/${segs}/${buildNumber}/execution/node/${encodeURIComponent(nodeId)}/wfapi/log`,
        { timeoutMs: LOG_REQUEST_TIMEOUT_MS },
      );
      return payload?.text ?? '';
    } catch (err) {
      const msg = (err as Error).message;
      if (msg.includes(' 404 ')) return null;
      throw err;
    }
  }

  async getTestReport(jobPath: string, buildNumber: number): Promise<TestReport | null> {
    const segs = jobPathToApiSegments(jobPath);
    try {
      const tree = 'failCount,skipCount,passCount,duration,suites[name,duration,cases[className,name,status,duration,errorDetails,errorStackTrace]]';
      return await this.request<TestReport>(
        'GET',
        `/${segs}/${buildNumber}/testReport/api/json?tree=${encodeURIComponent(tree)}`,
      );
    } catch (err) {
      const msg = (err as Error).message;
      if (msg.includes(' 404 ')) return null;
      throw err;
    }
  }

  async getQueue(): Promise<QueueItem[]> {
    const res = await this.request<{ items?: QueueItem[] }>('GET', '/queue/api/json?tree=items[id,url,why,inQueueSince,task[name,url]]');
    return res.items ?? [];
  }

  async listJobs(folderPath?: string, limit = 50): Promise<JobInfo[]> {
    const base = folderPath ? `/${jobPathToApiSegments(folderPath)}` : '';
    const range = Math.max(1, Math.min(200, limit));
    const tree = `jobs[name,fullName,url,_class,color,buildable,lastBuild[number,url],lastCompletedBuild[number,url]]{0,${range}}`;
    const res = await this.request<{ jobs?: JobInfo[] }>('GET', `${base}/api/json?tree=${encodeURIComponent(tree)}`);
    return res.jobs ?? [];
  }

  // ── Tool entry points ────────────────────────────────────────────────────

  async getBuildOverview(args: {
    jobPath: string;
    buildNumber?: BuildRef;
    sha?: string;
    scanLimit?: number;
    includeStages?: boolean;
    includeChangeSet?: boolean;
    includeArtifacts?: boolean;
    includeParameters?: boolean;
  }): Promise<ToolResult> {
    const {
      jobPath,
      includeStages = true,
      includeChangeSet = true,
      includeArtifacts = true,
      includeParameters = true,
    } = args;
    if (!jobPath) throw new Error('jobPath is required');

    let build: BuildSummary | null;
    if (args.sha) {
      build = await this.findBuildBySha(jobPath, args.sha, args.scanLimit ?? 30);
      if (!build) {
        return text(`No build found in last ${args.scanLimit ?? 30} builds of "${jobPath}" matching SHA ${args.sha}. Try increasing scanLimit, or pass buildNumber explicitly.`);
      }
    } else {
      const ref: BuildRef = args.buildNumber ?? 'lastBuild';
      build = await this.getBuild(jobPath, ref);
    }

    // Parallelize the optional pipeline fetch with the rest of the work
    const stagesPromise = includeStages
      ? this.getPipelineStages(jobPath, build.number).catch(() => null)
      : Promise.resolve(null);

    const lines: string[] = [];
    lines.push(`Job:    ${jobPath}`);
    lines.push(`Build:  #${build.number}${build.displayName && build.displayName !== `#${build.number}` ? ` (${build.displayName})` : ''}`);
    lines.push(`URL:    ${build.url}`);
    lines.push(`Result: ${fmtResult(build.result, build.building)}`);
    if (build.building && build.estimatedDuration) {
      const elapsed = Date.now() - build.timestamp;
      lines.push(`Progress: ${fmtDuration(elapsed)} elapsed of ~${fmtDuration(build.estimatedDuration)} estimated`);
    } else {
      lines.push(`Duration: ${fmtDuration(build.duration)}`);
    }
    lines.push(`Started: ${fmtTimestamp(build.timestamp)}`);

    const sha = extractCommitFromActions(build.actions);
    const branch = extractBranchFromActions(build.actions);
    if (sha)    lines.push(`Commit:  ${sha}${branch ? ` (${branch})` : ''}`);
    const cause = extractCauseSummary(build.actions);
    if (cause)  lines.push(`Cause:   ${cause}`);

    if (includeParameters) {
      const params = extractParameters(build.actions);
      if (params.length > 0) {
        lines.push('');
        lines.push('Parameters:');
        for (const p of params) {
          const v = p.value.length > 200 ? `${p.value.slice(0, 200)}...` : p.value;
          lines.push(`  ${p.name} = ${v}`);
        }
      }
    }

    const stages = await stagesPromise;
    if (stages && stages.stages.length > 0) {
      lines.push('');
      lines.push('Stages:');
      for (const s of stages.stages) {
        lines.push(`  ${s.status.padEnd(11)} ${fmtDuration(s.durationMillis).padStart(7)}  ${s.name}  [id=${s.id}]`);
        if (s.error?.message) {
          lines.push(`    ↳ ${s.error.message.split('\n')[0].slice(0, 200)}`);
        }
      }
      const failed = stages.stages.find((s) => s.status === 'FAILED');
      if (failed) {
        lines.push('');
        lines.push(`Tip: fetch just the failing stage's log with jenkins_get_log jobPath="${jobPath}" buildNumber=${build.number} stageId="${failed.id}".`);
      }
    }

    if (includeChangeSet) {
      const items = build.changeSet?.items ?? build.changeSets?.flatMap((cs) => cs.items ?? []) ?? [];
      if (items.length > 0) {
        lines.push('');
        lines.push(`Changes (${items.length}):`);
        for (const it of items.slice(0, 10)) {
          const short = it.commitId ? it.commitId.slice(0, 8) : '????????';
          const author = it.author?.fullName ?? 'unknown';
          const msg = (it.msg ?? '').split('\n')[0].slice(0, 100);
          lines.push(`  ${short}  ${author}: ${msg}`);
        }
        if (items.length > 10) lines.push(`  ... and ${items.length - 10} more`);
      }
    }

    if (includeArtifacts && build.artifacts && build.artifacts.length > 0) {
      lines.push('');
      lines.push(`Artifacts (${build.artifacts.length}):`);
      for (const a of build.artifacts.slice(0, 20)) {
        lines.push(`  ${build.url}artifact/${a.relativePath}`);
      }
      if (build.artifacts.length > 20) lines.push(`  ... and ${build.artifacts.length - 20} more`);
    }

    if (build.result === 'FAILURE' || build.result === 'UNSTABLE') {
      lines.push('');
      lines.push(`Tips:`);
      lines.push(`  • jenkins_get_tests jobPath="${jobPath}" buildNumber=${build.number}  (failed test names + stack traces)`);
      lines.push(`  • jenkins_get_log   jobPath="${jobPath}" buildNumber=${build.number}  (console log; tailChars caps size)`);
    }

    return text(lines.join('\n'));
  }

  async getLog(args: {
    jobPath: string;
    buildNumber: number;
    stageId?: string;
    tailChars?: number;
    full?: boolean;
  }): Promise<ToolResult> {
    const { jobPath, buildNumber, stageId, full } = args;
    if (!jobPath) throw new Error('jobPath is required');
    if (!buildNumber) throw new Error('buildNumber is required');
    const tailChars = full ? undefined : (args.tailChars ?? DEFAULT_LOG_TAIL_CHARS);

    if (stageId) {
      const details = await this.getPipelineStageDetails(jobPath, buildNumber, stageId);
      if (!details) {
        return text(`No pipeline stage with id "${stageId}" found for build #${buildNumber}. Use jenkins_get_build to list stage ids.`);
      }
      const nodes = details.stageFlowNodes ?? [];
      if (nodes.length === 0) {
        // Stage has no inner nodes; try fetching the stage's own log directly
        const stageLog = await this.getPipelineNodeLog(jobPath, buildNumber, stageId);
        if (!stageLog) return text(`Stage "${details.name}" has no logs.`);
        return text(maybeTrim(stageLog, tailChars));
      }
      const parts: string[] = [`Stage: ${details.name} (${details.status})`];
      for (const n of nodes) {
        const log = await this.getPipelineNodeLog(jobPath, buildNumber, n.id).catch(() => null);
        if (log && log.trim()) {
          parts.push('', `── ${n.name} [${n.status}] ──`);
          parts.push(log);
          if (n.error?.message) parts.push(`↳ ${n.error.message}`);
        } else if (n.error?.message) {
          parts.push('', `── ${n.name} [${n.status}] ──`, `↳ ${n.error.message}`);
        }
      }
      return text(maybeTrim(parts.join('\n'), tailChars) || '(no stage output)');
    }

    const log = await this.getConsoleLog(jobPath, buildNumber, { tailChars });
    return text(log || '(empty log)');
  }

  async listJob(args: { jobPath: string; limit?: number }): Promise<ToolResult> {
    const { jobPath, limit = 20 } = args;
    if (!jobPath) throw new Error('jobPath is required');
    const builds = await this.listBuilds(jobPath, limit);
    if (builds.length === 0) return text(`No builds found for "${jobPath}".`);

    const lines: string[] = [];
    lines.push(`${jobPath} — last ${builds.length} build${builds.length === 1 ? '' : 's'}:`);
    for (const b of builds) {
      const result = fmtResult(b.result, b.building).padEnd(8);
      const dur = b.building ? '...' : fmtDuration(b.duration);
      const when = fmtTimestamp(b.timestamp);
      const sha = extractCommitFromActions(b.actions);
      const branch = extractBranchFromActions(b.actions);
      const ref = sha ? ` ${sha.slice(0, 8)}${branch ? ` (${branch})` : ''}` : '';
      lines.push(`  #${String(b.number).padEnd(5)} ${result} ${dur.padStart(7)}  ${when}${ref}`);
    }
    return text(lines.join('\n'));
  }

  async listJobsTool(args: { folder?: string; limit?: number }): Promise<ToolResult> {
    const jobs = await this.listJobs(args.folder, args.limit ?? 50);
    if (jobs.length === 0) return text(args.folder ? `No jobs found under "${args.folder}".` : 'No top-level jobs found.');
    const lines: string[] = [];
    lines.push(`${args.folder ?? '(root)'}:`);
    for (const j of jobs) {
      const isFolder = KNOWN_FOLDER_CLASSES.has(j._class ?? '');
      const tag = isFolder ? '[folder]' : `[${j.color ?? '?'}]`;
      const last = j.lastBuild ? ` last #${j.lastBuild.number}` : '';
      lines.push(`  ${tag.padEnd(20)} ${j.fullName ?? j.name}${last}`);
    }
    return text(lines.join('\n'));
  }

  async getQueueTool(): Promise<ToolResult> {
    const items = await this.getQueue();
    if (items.length === 0) return text('Queue is empty.');
    const lines: string[] = [`Queue (${items.length} item${items.length === 1 ? '' : 's'}):`];
    for (const it of items) {
      const waited = it.inQueueSince ? fmtDuration(Date.now() - it.inQueueSince) : '?';
      lines.push(`  ${it.task?.name ?? '(unnamed)'} — waiting ${waited}${it.why ? ` — ${it.why}` : ''}`);
    }
    return text(lines.join('\n'));
  }

  async getTestsTool(args: {
    jobPath: string;
    buildNumber: number;
    failedOnly?: boolean;
    maxFailures?: number;
    includeStackTrace?: boolean;
  }): Promise<ToolResult> {
    const { jobPath, buildNumber, failedOnly = true, maxFailures = 25, includeStackTrace = true } = args;
    if (!jobPath) throw new Error('jobPath is required');
    if (!buildNumber) throw new Error('buildNumber is required');

    const report = await this.getTestReport(jobPath, buildNumber);
    if (!report) return text(`No test report for "${jobPath}" build #${buildNumber}. The build may not publish JUnit results.`);

    const fail = report.failCount ?? 0;
    const skip = report.skipCount ?? 0;
    const pass = report.passCount ?? 0;
    const total = fail + skip + pass;
    const lines: string[] = [];
    lines.push(`Tests for ${jobPath} #${buildNumber}: ${pass} passed, ${fail} failed, ${skip} skipped (total ${total})`);

    const failedCases: Array<{ suite: string; tc: TestCase }> = [];
    for (const suite of report.suites ?? []) {
      for (const tc of suite.cases ?? []) {
        if (tc.status === 'FAILED' || tc.status === 'REGRESSION') {
          failedCases.push({ suite: suite.name ?? '?', tc });
        }
      }
    }

    if (failedCases.length === 0 && fail === 0) {
      return text(lines.join('\n'));
    }

    if (failedCases.length === 0) {
      lines.push('');
      lines.push(`(${fail} failure${fail === 1 ? '' : 's'} reported but no per-case detail returned by Jenkins.)`);
      return text(lines.join('\n'));
    }

    lines.push('');
    lines.push(`Failed (${failedCases.length}${failedCases.length > maxFailures ? `, showing first ${maxFailures}` : ''}):`);
    for (const { tc } of failedCases.slice(0, maxFailures)) {
      const fqn = tc.className ? `${tc.className}.${tc.name ?? '?'}` : (tc.name ?? '?');
      lines.push('');
      lines.push(`✗ ${fqn}  (${fmtDuration((tc.duration ?? 0) * 1000)})`);
      if (tc.errorDetails) {
        const head = tc.errorDetails.split('\n').slice(0, 5).join('\n');
        lines.push(indent(head.length > 800 ? `${head.slice(0, 800)}...` : head, '    '));
      }
      if (includeStackTrace && tc.errorStackTrace) {
        const stack = tc.errorStackTrace.split('\n').slice(0, 15).join('\n');
        lines.push(indent(stack.length > 1200 ? `${stack.slice(0, 1200)}...` : stack, '    '));
      }
    }

    if (!failedOnly && pass > 0) {
      lines.push('');
      lines.push(`(failedOnly=false: passed/skipped cases not listed individually — set failedOnly=false in code if needed.)`);
    }

    return text(lines.join('\n'));
  }
}

function indent(s: string, prefix: string): string {
  return s.split('\n').map((line) => prefix + line).join('\n');
}

function maybeTrim(s: string, tailChars?: number): string {
  if (!tailChars || s.length <= tailChars) return s;
  return `... (truncated, last ${tailChars} of ${s.length} chars)\n` + s.slice(-tailChars);
}
