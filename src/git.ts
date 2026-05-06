import { execFileSync } from 'child_process';

function safeGit(args: string[], cwd?: string): string {
  try {
    return execFileSync('git', args, {
      cwd: cwd ?? process.cwd(),
      encoding: 'utf-8',
      stdio: ['ignore', 'pipe', 'ignore'],
    }).trim();
  } catch {
    return '';
  }
}

export function currentGitRemote(cwd?: string): string {
  return safeGit(['remote', 'get-url', 'origin'], cwd);
}

export function currentGitBranch(cwd?: string): string {
  return safeGit(['rev-parse', '--abbrev-ref', 'HEAD'], cwd);
}

export function currentGitSha(cwd?: string): string {
  return safeGit(['rev-parse', 'HEAD'], cwd);
}
