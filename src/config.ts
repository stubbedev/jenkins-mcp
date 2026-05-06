import { readFileSync, existsSync } from 'fs';
import { homedir } from 'os';
import { join, resolve } from 'path';

export interface Config {
  url: string;
  username: string;
  token: string;
  repoJobMap?: Record<string, string | string[]>;
}

interface ConfigFile {
  url?: string;
  username?: string;
  token?: string;
  repoJobMap?: Record<string, string | string[]>;
}

function readJsonFile(filePath: string): ConfigFile | null {
  try {
    if (!existsSync(filePath)) return null;
    return JSON.parse(readFileSync(filePath, 'utf-8')) as ConfigFile;
  } catch {
    return null;
  }
}

function getConfigPath(): string | null {
  const configArgIndex = process.argv.indexOf('--config');
  if (configArgIndex !== -1 && process.argv[configArgIndex + 1]) {
    return resolve(process.argv[configArgIndex + 1]);
  }
  if (process.env.JENKINS_MCP_CONFIG) {
    return resolve(process.env.JENKINS_MCP_CONFIG);
  }
  const homeConfig = join(homedir(), '.jenkins-mcp.json');
  if (existsSync(homeConfig)) return homeConfig;
  const cwdConfig = join(process.cwd(), '.jenkins-mcp.json');
  if (existsSync(cwdConfig)) return cwdConfig;
  return null;
}

export function loadConfig(): Config | null {
  const configPath = getConfigPath();
  const file = configPath ? readJsonFile(configPath) : null;

  const url = file?.url ?? process.env.JENKINS_URL ?? '';
  const username = file?.username ?? process.env.JENKINS_USERNAME ?? '';
  const token = file?.token ?? process.env.JENKINS_TOKEN ?? '';
  const repoJobMap = file?.repoJobMap;

  if (!url || !username || !token) {
    const missing: string[] = [];
    if (!url) missing.push('url (or JENKINS_URL)');
    if (!username) missing.push('username (or JENKINS_USERNAME)');
    if (!token) missing.push('token (or JENKINS_TOKEN)');
    console.error(`[jenkins-mcp] Disabled: missing ${missing.join(', ')}`);
    return null;
  }

  return { url, username, token, repoJobMap };
}
