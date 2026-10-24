import assert from 'node:assert/strict';
import { test } from 'node:test';
import { skipDownload } from './download.mjs';

test('skipDownload accepts only explicit switches', () => {
  for (const v of [undefined, '', '0', 'false', 'FALSE']) assert.equal(skipDownload(v), false, String(v));
  for (const v of ['1', 'true', 'True']) assert.equal(skipDownload(v), true, v);
  for (const v of ['yes', '2', 'skip']) {
    assert.throws(() => skipDownload(v), /JENKINS_MCP_SKIP_DOWNLOAD must be 1, true, 0 or false/, v);
  }
});
