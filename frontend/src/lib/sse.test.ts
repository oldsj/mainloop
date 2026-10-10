import assert from 'node:assert/strict';
import { test } from 'node:test';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import { transpileModule, ModuleKind } from 'typescript';

function clientModule(EventSource?: unknown) {
  const source = readFileSync(new URL('./sse.ts', import.meta.url), 'utf8').replace(
    "import { API_URL } from '$lib/config';",
    "const API_URL = 'http://fixture';"
  );
  const code = transpileModule(source, {
    compilerOptions: { module: ModuleKind.CommonJS }
  }).outputText;
  const context = { exports: {} as any, console, ...(EventSource ? { EventSource } : {}) };
  vm.runInNewContext(code, context);
  return context.exports;
}

test('SSE is safe during server rendering without EventSource', () => {
  const { SSEClient, connectSSE, disconnectSSE } = clientModule();
  const client = new SSEClient('http://fixture/events');
  assert.equal(client.isConnected(), false);
  assert.doesNotThrow(() => client.connect());
  assert.equal(client.isConnected(), false);
  assert.doesNotThrow(() => connectSSE());
  disconnectSSE();
});

test('browser SSE connects once and reports connection state', () => {
  let created = 0;
  let closed = 0;
  class Source {
    static OPEN = 1;
    readyState = 1;
    constructor() {
      created += 1;
    }
    addEventListener() {}
    close() {
      closed += 1;
    }
  }
  const { SSEClient } = clientModule(Source);
  const client = new SSEClient('http://fixture/events');
  assert.equal(client.isConnected(), false);
  client.connect();
  client.connect();
  assert.equal(created, 1);
  assert.equal(client.isConnected(), true);
  client.disconnect();
  assert.equal(closed, 1);
  assert.equal(client.isConnected(), false);
});
