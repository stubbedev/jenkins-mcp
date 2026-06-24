// Resolves (and, if needed, downloads) the prebuilt Go binary that matches the
// current platform. Shared by the postinstall script and the CLI launcher, so a
// failed install (e.g. offline) self-heals on first run.
import { createWriteStream } from 'node:fs';
import { chmod, mkdir, rm, stat } from 'node:fs/promises';
import { readFileSync } from 'node:fs';
import { Readable } from 'node:stream';
import { pipeline } from 'node:stream/promises';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = dirname(dirname(fileURLToPath(import.meta.url)));
const pkg = JSON.parse(readFileSync(join(root, 'package.json'), 'utf-8'));
const REPO = 'stubbedev/jenkins-mcp';

// Map Node's platform/arch onto the Go release asset naming. Anything not
// listed falls back to the "go install" hint in target().
const PLATFORMS = {
  'linux:x64': { os: 'linux', arch: 'amd64', ext: '' },
  'linux:arm64': { os: 'linux', arch: 'arm64', ext: '' },
  'linux:arm': { os: 'linux', arch: 'arm', ext: '' },
  'linux:ia32': { os: 'linux', arch: '386', ext: '' },
  'linux:ppc64': { os: 'linux', arch: 'ppc64le', ext: '' },
  'linux:s390x': { os: 'linux', arch: 's390x', ext: '' },
  'linux:riscv64': { os: 'linux', arch: 'riscv64', ext: '' },
  'darwin:x64': { os: 'darwin', arch: 'amd64', ext: '' },
  'darwin:arm64': { os: 'darwin', arch: 'arm64', ext: '' },
  'win32:x64': { os: 'windows', arch: 'amd64', ext: '.exe' },
  'win32:arm64': { os: 'windows', arch: 'arm64', ext: '.exe' },
  'win32:ia32': { os: 'windows', arch: '386', ext: '.exe' },
  'freebsd:x64': { os: 'freebsd', arch: 'amd64', ext: '' },
  'freebsd:arm64': { os: 'freebsd', arch: 'arm64', ext: '' },
};

function target() {
  const key = `${process.platform}:${process.arch}`;
  const t = PLATFORMS[key];
  if (!t) {
    throw new Error(
      `Unsupported platform ${key}. Install from source instead:\n` +
        `  go install github.com/${REPO}@v${pkg.version}`,
    );
  }
  return t;
}

function binDir() {
  return join(root, 'bin');
}

function binPath() {
  const { ext } = target();
  return join(binDir(), `jenkins-mcp${ext}`);
}

async function fileExists(p) {
  try {
    const s = await stat(p);
    return s.isFile() && s.size > 0;
  } catch {
    return false;
  }
}

// ensureBinary returns the path to the platform binary, downloading it from the
// matching GitHub release on first use. Idempotent and safe to call repeatedly.
export async function ensureBinary() {
  const dest = binPath();
  if (await fileExists(dest)) return dest;

  const { os, arch, ext } = target();
  const asset = `jenkins-mcp_${os}_${arch}${ext}`;
  const url = `https://github.com/${REPO}/releases/download/v${pkg.version}/${asset}`;

  const res = await fetch(url, { redirect: 'follow' });
  if (!res.ok || !res.body) {
    throw new Error(`Failed to download ${asset} (v${pkg.version}): HTTP ${res.status}`);
  }

  await mkdir(binDir(), { recursive: true });
  const tmp = `${dest}.tmp`;
  await rm(tmp, { force: true });
  await pipeline(Readable.fromWeb(res.body), createWriteStream(tmp));
  await chmod(tmp, 0o755);
  // Atomic-ish: rename into place once fully written.
  const { rename } = await import('node:fs/promises');
  await rename(tmp, dest);
  return dest;
}
