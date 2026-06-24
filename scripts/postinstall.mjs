// Best-effort download of the prebuilt binary at install time. Failures are not
// fatal — the CLI launcher retries the download on first run — so CI / offline
// environments still succeed.
import { ensureBinary } from './download.mjs';

if (process.env.JENKINS_MCP_SKIP_DOWNLOAD === '1') {
  process.exit(0);
}

try {
  await ensureBinary();
} catch (err) {
  console.error(`[jenkins-mcp] postinstall: ${err.message}`);
  console.error('[jenkins-mcp] the binary will be fetched on first run instead.');
}
