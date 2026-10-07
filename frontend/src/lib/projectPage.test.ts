import assert from 'node:assert/strict';
import { test } from 'node:test';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { compile } from 'svelte/compiler';
import { render } from 'svelte/server';
import { transpileModule, ModuleKind } from 'typescript';

test('a failed project detail read renders an error and retry instead of loading forever', async () => {
  const source = await readFile(
    new URL('../routes/projects/[id]/+page.svelte', import.meta.url),
    'utf8'
  );
  const dir = await mkdtemp(resolve('node_modules/.project-page-render-'));
  try {
    // Stub route/API boundaries; render the actual page and use the actual project store.
    await writeFile(
      resolve(dir, 'boundaries.mjs'),
      `
      import { writable } from 'svelte/store';
      export const page = writable({ params: { id: 'project-id' } });
      export const api = {
        getProjectDetail: async () => { throw new Error('Failed to get project detail'); }
      };
      export const agentLabel = (kind) => kind ?? 'agent';
      export const goto = () => {};
      export const statusLabel = (status) => status;
      export default function MergePolicy() {}
    `
    );
    const store = await readFile(new URL('./stores/projects.ts', import.meta.url), 'utf8');
    await writeFile(
      resolve(dir, 'projects.mjs'),
      transpileModule(store.replace("'$lib/api'", "'./boundaries.mjs'"), {
        compilerOptions: { module: ModuleKind.ESNext }
      }).outputText
    );
    const compiled = compile(source, { filename: 'ProjectPage.svelte', generate: 'server' });
    await writeFile(
      resolve(dir, 'page.mjs'),
      compiled.js.code
        .replaceAll("'$lib/stores/projects'", "'./projects.mjs'")
        .replace(/'(?:\$app\/[^']+|\$lib\/[^']+)'/g, "'./boundaries.mjs'")
    );
    const component = (await import(pathToFileURL(resolve(dir, 'page.mjs')).href)).default;
    const { projects } = await import(pathToFileURL(resolve(dir, 'projects.mjs')).href);

    assert.match(render(component).body, /Loading project\.\.\./);
    await projects.fetchProjectDetail('project-id');
    const html = render(component).body;
    assert.match(html, /role="alert"[^>]*>Failed to get project detail/);
    assert.match(html, /Retry loading project/);
    assert.doesNotMatch(html, /Loading project\.\.\./);

    const { api } = await import(pathToFileURL(resolve(dir, 'boundaries.mjs')).href);
    let failRetry!: (error: Error) => void;
    api.getProjectDetail = () =>
      new Promise((_, reject) => {
        failRetry = reject;
      });
    const retry = projects.fetchProjectDetail('project-id');
    const retryHtml = render(component).body;
    assert.match(retryHtml, /Loading project\.\.\./);
    assert.doesNotMatch(retryHtml, /role="alert"/);
    failRetry(new Error('Failed to get project detail'));
    await retry;
    assert.match(render(component).body, /Retry loading project/);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});
