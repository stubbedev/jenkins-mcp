// Best-effort download of the prebuilt binary at install time. Failures are not
// fatal — the CLI launcher retries the download on first run — so installs in
// offline/CI environments still succeed.
import { ensureBinary, skipDownload } from './download.mjs';

let skip;
try {
  skip = skipDownload();
} catch (err) {
  console.error(`[jenkins-mcp] postinstall: ${err.message}`);
  process.exit(1);
}
if (skip) {
  process.exit(0);
}

try {
  await ensureBinary();
} catch (err) {
  console.error(`[jenkins-mcp] postinstall: ${err.message}`);
  console.error('[jenkins-mcp] The binary will be fetched on first run instead.');
}
