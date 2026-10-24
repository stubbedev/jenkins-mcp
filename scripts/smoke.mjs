import { spawn } from 'node:child_process';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { createInterface } from 'node:readline';
import { fileURLToPath } from 'node:url';

const root = dirname(dirname(fileURLToPath(import.meta.url)));
const binary = join(root, process.platform === 'win32' ? 'jenkins-mcp.exe' : 'jenkins-mcp');
const fixture = join(root, 'testdata', 'smoke-config.json');
const timeoutMs = 10_000;

const requests = [
  { jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocolVersion: '2024-11-05', capabilities: {}, clientInfo: { name: 'smoke', version: '1' } } },
  { jsonrpc: '2.0', method: 'notifications/initialized' },
  { jsonrpc: '2.0', id: 2, method: 'tools/list', params: {} },
];

function listTools(cwd) {
  return new Promise((resolve, reject) => {
    const env = { PATH: process.env.PATH, HOME: cwd, JENKINS_MCP_CONFIG: fixture };
    if (process.env.SYSTEMROOT) env.SYSTEMROOT = process.env.SYSTEMROOT;
    const child = spawn(binary, [], { cwd, env, stdio: ['pipe', 'pipe', 'pipe'] });
    let stderr = '';
    let settled = false;
    const finish = (err, tools) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      child.stdin.end();
      child.kill();
      if (err) reject(new Error(`${err.message}${stderr ? `\nserver stderr:\n${stderr}` : ''}`));
      else resolve(tools);
    };
    const timer = setTimeout(() => finish(new Error(`no tools/list response within ${timeoutMs}ms`)), timeoutMs);
    child.stderr.on('data', (chunk) => { stderr += chunk; });
    child.on('error', (err) => finish(new Error(`starting ${binary}: ${err.message}`)));
    child.on('exit', (code, signal) => finish(new Error(`server exited (code ${code}, signal ${signal}) before answering tools/list`)));
    createInterface({ input: child.stdout }).on('line', (line) => {
      let msg;
      try {
        msg = JSON.parse(line);
      } catch (err) {
        finish(new Error(`server wrote a non-JSON line to stdout: ${line}`));
        return;
      }
      if (msg.id !== 2) return;
      if (msg.error) finish(new Error(`server error: ${msg.error.message}`));
      else finish(null, msg.result && msg.result.tools);
    });
    child.stdin.write(requests.map((r) => JSON.stringify(r)).join('\n') + '\n');
  });
}

const cwd = await mkdtemp(join(tmpdir(), 'jenkins-mcp-smoke-'));
try {
  const tools = await listTools(cwd);
  if (!Array.isArray(tools) || tools.length === 0) throw new Error('no tools in the tools/list response');
  const bad = tools.filter((t) => !t.name || !t.inputSchema);
  if (bad.length) throw new Error(`malformed tools: ${bad.map((t) => t.name || '(unnamed)').join(', ')}`);
  console.log(`smoke OK — ${tools.length} tool(s): ${tools.map((t) => t.name).join(', ')}`);
} catch (err) {
  console.error('smoke FAIL:', err.message);
  process.exitCode = 1;
} finally {
  await rm(cwd, { recursive: true, force: true });
}
