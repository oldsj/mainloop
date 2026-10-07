import assert from 'node:assert/strict';
import test from 'node:test';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { compile } from 'svelte/compiler';
import { render } from 'svelte/server';
import { transpileModule, ModuleKind } from 'typescript';
import { view, reassignedView, operation, CLAUDE_PROFILE, CODEX_PROFILE } from './taskFixtures.ts';
import * as actions from './taskActions.ts';
import * as state from './taskState.ts';
import * as events from './taskEvents.ts';

const recoveryGlobal = globalThis as typeof globalThis & {
  __taskRecovery?: { actions: typeof actions; state: typeof state; events: typeof events };
};

test('lost reply remains resendable after eligibility and current provider change', async () => {
  const dir = await mkdtemp(resolve('node_modules/.task-recovery-'));
  try {
    const boundaries = pathToFileURL(resolve(dir, 'boundaries.mjs')).href;
    await writeFile(
      resolve(dir, 'boundaries.mjs'),
      `
      export const api = {};
      export const getSSEClient = () => ({ isConnected: () => false, on: () => () => {} });
    `
    );
    const b = await import(boundaries);
    let current = view();
    const sent: unknown[] = [];
    b.api.getTask = async () => current;
    b.api.listProviders = async () => [CLAUDE_PROFILE, CODEX_PROFILE];
    b.api.reassignTask = async (_id: string, body: unknown) => {
      sent.push(body);
      if (sent.length === 1) {
        current = reassignedView();
        current.task.id = 'task-1';
        current.actions = { reassign: { available: false, reason: 'handoff' } };
        throw new TypeError('lost reply');
      }
      return operation();
    };
    // Compile the real store with only its API/SSE boundaries substituted.
    recoveryGlobal.__taskRecovery = { actions, state, events };
    await writeFile(
      resolve(dir, 'logic.mjs'),
      `
      export const { createActionTracker, actionAvailability, reassignTargets } = globalThis.__taskRecovery.actions;
      export const { normalizeView, currentAttempt, switchBlockers } = globalThis.__taskRecovery.state;
      export const { createTaskSync, mergeView } = globalThis.__taskRecovery.events;
    `
    );
    const source = await readFile(new URL('./stores/tasks.ts', import.meta.url), 'utf8');
    await writeFile(
      resolve(dir, 'store.mjs'),
      transpileModule(
        source
          .replace(/'\$lib\/(api|sse)'/g, "'./boundaries.mjs'")
          .replace(/'\$lib\/(taskActions|taskEvents|taskState)'/g, "'./logic.mjs'"),
        { compilerOptions: { module: ModuleKind.ESNext } }
      ).outputText
    );
    const { tasks } = await import(pathToFileURL(resolve(dir, 'store.mjs')).href);
    let snapshot: any;
    const off = tasks.subscribe((s: any) => {
      snapshot = s;
    });
    await tasks.fetchTask('task-1');
    await tasks.loadProfiles();
    await tasks.act('reassign', 'task-1', { targetProfileId: 'codex-default' });
    assert.equal(snapshot.actions['task-1'].pending.body.target_profile_id, 'codex-default');
    assert.equal(actions.actionAvailability(snapshot.byId['task-1'], 'reassign').enabled, false);
    assert.deepEqual(
      actions
        .reassignTargets(
          snapshot.profiles,
          snapshot.byId['task-1'],
          state.currentAttempt(snapshot.byId['task-1'])
        )
        .map((p) => p.id),
      ['claude-default']
    );

    const componentSource = await readFile(
      new URL('./components/TaskActions.svelte', import.meta.url),
      'utf8'
    );
    await writeFile(
      resolve(dir, 'component.mjs'),
      compile(componentSource, {
        filename: 'TaskActions.svelte',
        generate: 'server'
      })
        .js.code.replace("'$lib/stores/tasks'", "'./store.mjs'")
        .replace(/'\$lib\/(taskActions|taskState)'/g, "'./logic.mjs'")
    );
    const component = (await import(pathToFileURL(resolve(dir, 'component.mjs')).href)).default;
    const html = render(component, { props: { view: snapshot.byId['task-1'] } }).body;
    assert.match(html, /Original reassign: version 3/);
    assert.match(html, /attempt attempt-1/);
    assert.match(html, /target codex-default/);
    assert.match(
      html,
      /<button type="button"(?![^>]* disabled(?:[ =/>]))[^>]*>Resend original request/
    );

    let finish!: (value: ReturnType<typeof operation>) => void;
    b.api.reassignTask = async (_id: string, body: unknown) => {
      sent.push(body);
      return new Promise((resolve) => {
        finish = resolve;
      });
    };
    const replay = tasks.resend('task-1');
    const busyHtml = render(component, { props: { view: snapshot.byId['task-1'] } }).body;
    assert.match(busyHtml, /<button type="button" disabled[^>]*>Resend original request/);
    assert.equal(await tasks.resend('task-1'), null);
    assert.equal(sent.length, 2);
    finish(operation());
    const outcome = await replay;
    assert.equal(outcome.status, 'accepted');
    assert.deepEqual(sent[1], sent[0]);
    assert.equal(snapshot.actions['task-1'].pending, null);
    off();
  } finally {
    delete recoveryGlobal.__taskRecovery;
    await rm(dir, { recursive: true, force: true });
  }
});

test('list rows show unknown for incomplete or expired CI evidence for every state', async () => {
  const dir = await mkdtemp(resolve('node_modules/.task-list-'));
  try {
    recoveryGlobal.__taskRecovery = { actions, state, events };
    await writeFile(
      resolve(dir, 'state.mjs'),
      `
      export const { buildTree, openOperations, providerLabel, publicationFacts,
        reasonLabel, statusLabel, statusTone, toneClass } = globalThis.__taskRecovery.state;
    `
    );
    const source = await readFile(new URL('./components/TaskList.svelte', import.meta.url), 'utf8');
    await writeFile(
      resolve(dir, 'list.mjs'),
      compile(source, {
        filename: 'TaskList.svelte',
        generate: 'server'
      }).js.code.replace("'$lib/taskState'", "'./state.mjs'")
    );
    const component = (await import(pathToFileURL(resolve(dir, 'list.mjs')).href)).default;
    const fresh = new Date().toISOString();
    for (const ci_state of ['success', 'failure', 'pending'] as const) {
      for (const evidence of [
        {},
        { pr_head_sha: 'aaa', observed_at: fresh },
        { ci_head_sha: 'aaa', observed_at: fresh },
        { pr_head_sha: 'aaa', ci_head_sha: 'aaa' },
        { pr_head_sha: 'aaa', ci_head_sha: 'bbb', observed_at: fresh },
        { pr_head_sha: 'aaa', ci_head_sha: 'aaa', observed_at: 'invalid' },
        { pr_head_sha: 'aaa', ci_head_sha: 'aaa', observed_at: '2020-01-01T00:00:00Z' }
      ]) {
        const html = render(component, {
          props: {
            views: [
              view(
                {},
                {
                  projection: { ci_state, ...evidence }
                }
              )
            ]
          }
        }).body;
        assert.match(html, /CI unknown/);
        assert.doesNotMatch(html, /CI (passing|failing|running)/);
      }
      const html = render(component, {
        props: {
          views: [
            view(
              {},
              {
                projection: { ci_state, pr_head_sha: 'aaa', ci_head_sha: 'aaa', observed_at: fresh }
              }
            )
          ]
        }
      }).body;
      const expected = { success: 'passing', failure: 'failing', pending: 'running' }[ci_state];
      assert.ok(html.includes('CI ' + expected));
    }
  } finally {
    delete recoveryGlobal.__taskRecovery;
    await rm(dir, { recursive: true, force: true });
  }
});

test('background task failures warn on cached detail and only that task success clears them', async () => {
  const dir = await mkdtemp(resolve('node_modules/.task-read-recovery-'));
  try {
    recoveryGlobal.__taskRecovery = { actions, state, events };
    await writeFile(
      resolve(dir, 'boundaries.mjs'),
      `
      import { writable } from 'svelte/store';
      export const page = writable({ params: { id: 'task-1' } });
      export const api = {};
      export const listeners = {};
      export const getSSEClient = () => ({
        isConnected: () => true,
        on: (kind, callback) => {
          listeners[kind] = callback;
          return () => { delete listeners[kind]; };
        }
      });
      export default function TaskDetail() {}
    `
    );
    await writeFile(
      resolve(dir, 'logic.mjs'),
      `
      export const { createActionTracker } = globalThis.__taskRecovery.actions;
      export const { normalizeView } = globalThis.__taskRecovery.state;
      export const { createTaskSync, mergeView } = globalThis.__taskRecovery.events;
    `
    );
    const source = await readFile(new URL('./stores/tasks.ts', import.meta.url), 'utf8');
    await writeFile(
      resolve(dir, 'store.mjs'),
      transpileModule(
        source
          .replace(/'\$lib\/(api|sse)'/g, "'./boundaries.mjs'")
          .replace(/'\$lib\/(taskActions|taskEvents|taskState)'/g, "'./logic.mjs'"),
        { compilerOptions: { module: ModuleKind.ESNext } }
      ).outputText
    );
    const b = await import(pathToFileURL(resolve(dir, 'boundaries.mjs')).href);
    let fail = false;
    b.api.getTask = async () => {
      if (fail) throw new Error('offline refresh');
      return view();
    };
    b.api.listTasks = async () => [view({ id: 'other-task' })];
    b.api.retryTask = async () => operation({ kind: 'retry' });
    const { tasks } = await import(pathToFileURL(resolve(dir, 'store.mjs')).href);
    let snapshot: any;
    const off = tasks.subscribe((s: any) => {
      snapshot = s;
    });
    await tasks.fetchTask('task-1');
    await tasks.fetchList();
    const pageSource = await readFile(
      new URL('../routes/tasks/[id]/+page.svelte', import.meta.url),
      'utf8'
    );
    await writeFile(
      resolve(dir, 'page.mjs'),
      compile(pageSource, {
        filename: 'TaskPage.svelte',
        generate: 'server'
      })
        .js.code.replace("'$lib/stores/tasks'", "'./store.mjs'")
        .replace(/'(?:\$app\/[^']+|\$lib\/components\/[^']+)'/g, "'./boundaries.mjs'")
    );
    const component = (await import(pathToFileURL(resolve(dir, 'page.mjs')).href)).default;
    const html = () => render(component).body;
    assert.doesNotMatch(html(), /Showing the last known state/);

    // Cover SSE, reconnect and post-action reads using the real store.
    for (const trigger of [
      async () => {
        b.listeners['task:updated']({
          data: { event_id: 'background-1', task_id: 'task-1', version: 4 }
        });
        await new Promise((resolve) => setImmediate(resolve));
      },
      async () => {
        b.listeners.connected();
        await new Promise((resolve) => setImmediate(resolve));
      },
      async () => {
        await tasks.act('retry', 'task-1');
      }
    ]) {
      fail = true;
      await trigger();
      assert.ok(snapshot.byId['task-1']);
      assert.ok(snapshot.taskErrors['task-1']);
      assert.equal(snapshot.error, null);
      assert.match(html(), /Showing the last known state/);
      assert.match(html(), /<button type="button"[^>]*>Retry loading/);

      await tasks.fetchList();
      assert.ok(snapshot.taskErrors['task-1']);
      b.api.listTasks = async () => {
        throw new Error('list offline');
      };
      await tasks.fetchList();
      assert.equal(snapshot.error, 'list offline');
      assert.ok(snapshot.taskErrors['task-1']);
      b.api.listTasks = async () => [view({ id: 'other-task' })];

      fail = false;
      await tasks.refresh('task-1');
      assert.equal(snapshot.taskErrors['task-1'], undefined);
      assert.doesNotMatch(html(), /Showing the last known state|Retry loading/);
      assert.equal(snapshot.error, 'list offline');
      await tasks.fetchList();
      assert.equal(snapshot.error, null);
    }
    off();
  } finally {
    delete recoveryGlobal.__taskRecovery;
    await rm(dir, { recursive: true, force: true });
  }
});
