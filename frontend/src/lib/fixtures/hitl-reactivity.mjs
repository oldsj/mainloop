// Execute the actual component's script, event handlers and view expressions with Svelte's
// client compiler/runtime. No DOM or network refresh can mask a missing invalidation.
import assert from 'node:assert/strict';
import { readFile, writeFile, mkdtemp, rm } from 'node:fs/promises';
import { resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { parse, compileModule } from 'svelte/compiler';
import ts from 'typescript';
import { effect_root, render_effect } from 'svelte/internal/client';
import { flushSync } from 'svelte';

const source = await readFile(new URL('../components/HITLRequest.svelte', import.meta.url), 'utf8');
const nodes = [];
function walk(value) {
  if (!value || typeof value !== 'object') return;
  if (value.type) nodes.push(value);
  for (const child of Object.values(value)) {
    if (Array.isArray(child)) child.forEach(walk);
    else if (child && typeof child === 'object') walk(child);
  }
}
walk(parse(source, { modern: true }));
const button = (text) =>
  nodes.find(
    (n) =>
      n.type === 'RegularElement' &&
      n.name === 'button' &&
      n.fragment.nodes.some((c) => c.type === 'Text' && c.data.trim() === text)
  );
const expression = (node, attr) => {
  const value = node.attributes.find((a) => a.name === attr).value;
  const expr = (Array.isArray(value) ? value[0] : value).expression;
  return source.slice(expr.start, expr.end);
};
const approve = button('Approve'),
  reject = button('Reject'),
  send = button('Send response');
// Send contains a conditional label; select it by the actual submit class instead.
const submit =
  send ??
  nodes.find(
    (n) =>
      n.type === 'RegularElement' &&
      n.name === 'button' &&
      n.attributes.some((a) => a.name === 'class' && a.value?.[0]?.data === 'submit')
  );
const reason = nodes.find((n) => n.type === 'RegularElement' && n.name === 'textarea');
const reasonIf = nodes.find(
  (n) =>
    n.type === 'IfBlock' &&
    source.slice(n.test.start, n.test.end) === "decisions[tool.id] === 'reject'"
);
const script = source.match(/<script lang="ts">([\s\S]*?)<\/script>/)[1];
const statements = ts.createSourceFile(
  'request.ts',
  script,
  ts.ScriptTarget.Latest,
  true,
  ts.ScriptKind.TS
).statements;
const imports = statements
  .filter(ts.isImportDeclaration)
  .map((n) => n.getText())
  .join('\n')
  .replace("'../hitl'", JSON.stringify(new URL('../hitl.ts', import.meta.url).href));
const body = statements
  .filter((n) => !ts.isImportDeclaration(n) && !n.getText().includes('$props()'))
  .map((n) => n.getText())
  .join('\n');
const harness = `${imports}
export function createHarness(snapshot) {
  ${body}
  return {
    approve(id) { const tool = { id }; (${expression(approve, 'onclick')})(); },
    reject(id) { const tool = { id }; (${expression(reject, 'onclick')})(); },
    reason(id, text) { const tool = { id }; (${expression(reason, 'oninput')})({ currentTarget: { value: text } }); },
    inspect(id) { const tool = { id }; return {
      approved: ${expression(approve, 'aria-pressed')}, rejected: ${expression(reject, 'aria-pressed')},
      reasonVisible: ${source.slice(reasonIf.test.start, reasonIf.test.end)}, disabled: ${expression(submit, 'disabled')},
      reason: Object.hasOwn(reasons, id) ? reasons[id] : '', validation
    }; },
    response() { return buildHITLResponse(view.request.payload, { decisions, reasons, answers }); }
  };
}`;
const js = ts.transpile(harness, { target: ts.ScriptTarget.ESNext, module: ts.ModuleKind.ESNext });
const compiled = compileModule(js, { filename: 'hitl-reactivity.svelte.js', generate: 'client' }).js
  .code;
const dir = await mkdtemp(resolve('node_modules/.hitl-reactivity-'));
try {
  const file = resolve(dir, 'harness.mjs');
  await writeFile(file, compiled);
  const { createHarness } = await import(pathToFileURL(file).href);
  const snapshot = {
    busy: false,
    stale: false,
    uncertain: false,
    view: {
      answerable: true,
      writes_enabled: true,
      response: null,
      request: {
        payload: {
          type: 'tool_approval_request',
          tools: ['__proto__', 'second'].map((id) => ({ id, call_id: id, name: 'tool', args: {} }))
        }
      }
    }
  };
  let draft, observed;
  const stop = effect_root(() => {
    draft = createHarness(snapshot);
    render_effect(() => {
      observed = draft.inspect('__proto__');
    });
  });
  try {
    assert.equal(observed.disabled, true);
    draft.reject('__proto__');
    flushSync();
    assert.equal(observed.rejected, true);
    assert.equal(observed.approved, false);
    assert.equal(observed.reasonVisible, true);
    assert.equal(observed.disabled, true); // second call still has no decision
    draft.approve('second');
    flushSync();
    assert.equal(observed.disabled, false);
    draft.reason('__proto__', 'Keep the tests');
    flushSync();
    assert.equal(observed.reason, 'Keep the tests');
    assert.equal(draft.response().approvals[0].rejection_reason, 'Keep the tests');
    draft.approve('__proto__');
    flushSync();
    assert.equal(observed.approved, true);
    assert.equal(observed.rejected, false);
    assert.equal(observed.reasonVisible, false);
    assert.equal(observed.reason, '');
    assert.equal(observed.disabled, false);
    assert.equal(draft.response().approvals[0].rejection_reason, undefined);
  } finally {
    stop();
  }
} finally {
  await rm(dir, { recursive: true, force: true });
}
