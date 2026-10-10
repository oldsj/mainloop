import assert from 'node:assert/strict';
import test from 'node:test';
import { choiceScope } from './healthFilters.ts';

test('a session or task entry link does not become the option inventory', () => {
  const entry = choiceScope({ session_id: 'session-a', hours: 24 });
  assert.equal(entry.narrowed, true);
  // Options load from the wider scope, so other sessions stay selectable.
  assert.deepEqual(entry.filters, { project_id: '', provider: '', hours: 24 });
  assert.equal(choiceScope({ task_id: 'task-a', hours: 24 }).key, entry.key);
  assert.equal(choiceScope({ hours: 24 }).narrowed, false);
});

test('changing the project, provider or window changes the option inventory', () => {
  const base = choiceScope({ project_id: 'p1', hours: 24 }).key;
  assert.notEqual(choiceScope({ project_id: 'p2', hours: 24 }).key, base);
  assert.notEqual(choiceScope({ project_id: 'p1', provider: 'claude', hours: 24 }).key, base);
  assert.notEqual(choiceScope({ project_id: 'p1', hours: 168 }).key, base);
});
