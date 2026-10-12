/**
 * Health fixtures in the server's shape. Each builder returns a valid record; a test overrides
 * only the field it is about, so the rest of the record stays readable and valid.
 */
export function totals(overrides: Record<string, unknown> = {}) {
  return {
    turns: 4,
    usage_observed_turns: 3,
    usage_complete_turns: 2,
    input_tokens: 1200,
    output_tokens: 300,
    failures: 2,
    repeated_failure_groups: 1,
    unresolved_calls: 0,
    coverage: 'complete',
    ...overrides
  };
}

export function session(overrides: Record<string, unknown> = {}) {
  return {
    session_id: 'session-1',
    title: 'Retry work',
    provider: 'claude',
    project_id: 'project-1',
    task_id: 'task-1',
    attempt_id: 'attempt-1',
    task_title: 'Add retry button',
    totals: totals(),
    ...overrides
  };
}

export function healthRow(overrides: Record<string, unknown> = {}) {
  return {
    id: 'row-1',
    task_id: 'task-1',
    project_id: 'project-1',
    title: 'Add retry button',
    sessions: [session()],
    turns: [],
    totals: totals(),
    reports: [],
    approvals: [],
    gaps: [],
    expected_receipts: 1,
    ...overrides
  };
}

export function healthView(rows: unknown[], overrides: Record<string, unknown> = {}) {
  return {
    rows,
    hours: 24,
    partial: false,
    unsupported: ['cache', 'exact_approval_wait', 'codex_usage', 'native_child_attribution'],
    retention: { events_days: 7, summaries_days: 90 },
    ...overrides
  };
}

/** A tool observation. Turn and usage observations have the same envelope with no `tool`. */
export function toolEvent(overrides: Record<string, unknown> = {}) {
  return {
    event_id: 'event-1',
    task_id: 'task-1',
    attempt_id: 'attempt-1',
    received_at: '2026-10-10T10:00:00Z',
    payload: {
      kind: 'tool',
      sequence: 3,
      source: 'claude_result_stream',
      tool: {
        tool_class: 'shell',
        outcome: 'error',
        category: 'tool_error',
        path: 'src/app.ts'
      }
    },
    ...overrides
  };
}

export function events(list: unknown[], partial = false) {
  return { events: list, partial };
}
