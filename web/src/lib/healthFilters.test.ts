import { expect, test } from 'vitest';
import { choiceScope } from './healthFilters';

test('a session or task entry link does not become the option inventory', () => {
  const entry = choiceScope({ session_id: 'session-a', hours: 24 });
  expect(entry.narrowed).toBe(true);
  // Options load from the wider scope, so other sessions stay selectable.
  expect(entry.filters).toStrictEqual({ project_id: '', provider: '', hours: 24 });
  expect(choiceScope({ task_id: 'task-a', hours: 24 }).key).toBe(entry.key);
  expect(choiceScope({ hours: 24 }).narrowed).toBe(false);
});

test('changing the project, provider or window changes the option inventory', () => {
  const base = choiceScope({ project_id: 'p1', hours: 24 }).key;
  expect(choiceScope({ project_id: 'p2', hours: 24 }).key).not.toBe(base);
  expect(choiceScope({ project_id: 'p1', provider: 'claude', hours: 24 }).key).not.toBe(base);
  expect(choiceScope({ project_id: 'p1', hours: 168 }).key).not.toBe(base);
});
