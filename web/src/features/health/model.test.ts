import { describe, expect, it } from 'vitest';
import {
  approvalText,
  attemptChoices,
  countText,
  eventLines,
  filtersFrom,
  gapText,
  healthQuery,
  parseRows,
  projectChoices,
  readObservations,
  readView,
  rowTitle,
  searchFrom,
  sessionChoices,
  sortByFailures,
  taskChoices,
  windowSummary,
  withSelected,
  type HealthRow
} from './model';
import { events, healthRow, healthView, session, toolEvent, totals } from './test/fixtures';

const row = (overrides: Record<string, unknown>) => healthRow(overrides) as unknown as HealthRow;

describe('sortByFailures', () => {
  it('puts the most failures first and unknown counts last, keeping ties in order', () => {
    const rows = [
      row({ id: 'a', totals: totals({ failures: 1 }) }),
      row({ id: 'b', totals: totals({ failures: null }) }),
      row({ id: 'c', totals: totals({ failures: 5 }) }),
      row({ id: 'd', totals: totals({ failures: 1 }) })
    ];
    expect(sortByFailures(rows).map((item) => item.id)).toEqual(['c', 'a', 'd', 'b']);
  });
});

describe('readView', () => {
  it('reads a valid view and counts the rows it had to drop', () => {
    const view = readView(
      healthView([healthRow({ id: 'good' }), healthRow({ id: 'bad', totals: 'nope' })], {
        partial: true
      })
    );
    expect(view?.rows.map((item) => item.id)).toEqual(['good']);
    expect(view?.dropped).toBe(1);
    expect(view?.partial).toBe(true);
  });

  it('returns null when the envelope itself is not readable', () => {
    expect(readView({ rows: 'nope', hours: 24, partial: false, retention: {} })).toBeNull();
    expect(readView(null)).toBeNull();
  });
});

describe('parseRows', () => {
  const report = { id: 'report-1', task_id: 'task-1', attempt_id: 'attempt-1', outcome: 'failed' };

  it('keeps a row whose claims are partly malformed, with its totals, and counts the bad claims', () => {
    const { rows, dropped } = parseRows([
      healthRow({
        totals: totals({ failures: 4 }),
        reports: [report, { id: 'report-2' }],
        // No runtime_session_id, so this approval cannot be read.
        approvals: [{ id: 'approval-bad', state: 'pending' }]
      })
    ]);

    const row = rows[0];
    expect(dropped).toBe(0);
    expect(row).toBeDefined();
    expect(row?.totals.failures).toBe(4);
    expect(row?.reports).toEqual([report]);
    expect(row?.approvals).toEqual([]);
    expect(row?.unreadableClaims).toBe(2);
  });

  it('drops a row whose own fields cannot be read, even when its claims can', () => {
    const { rows, dropped } = parseRows([healthRow({ totals: 'nope', reports: [report] })]);

    expect(rows).toEqual([]);
    expect(dropped).toBe(1);
  });

  it('drops a repeated row id and counts it apart from unreadable rows, keeping the first row', () => {
    const { rows, dropped, repeated } = parseRows([
      healthRow({ id: 'same', title: 'First' }),
      healthRow({ id: 'same', title: 'Second' })
    ]);

    expect(repeated).toBe(1);
    expect(dropped).toBe(0);
    expect(rows.map((item) => item.title)).toEqual(['First']);
  });
});

describe('row copy', () => {
  it('shows Unavailable for an unknown count and Unassigned for a row with no task', () => {
    expect(countText(null)).toBe('Unavailable');
    expect(countText(1200)).toBe('1,200');
    expect(rowTitle(row({ title: null, task_id: null }))).toBe('Unassigned');
    expect(rowTitle(row({ title: null, task_id: 'task-9' }))).toBe('task-9');
  });

  it('reads gap names and claim state in plain words', () => {
    expect(gapText(['tool_outcomes', 'approval_waits'])).toBe(
      'Collection gaps: tool outcomes, approval waits.'
    );
    expect(approvalText({ id: 'h1', runtime_session_id: null, state: null })).toBe(
      'Approval unknown; exact wait unavailable.'
    );
  });

  it('falls back to a plain window label for an unlisted value', () => {
    expect(windowSummary(168)).toBe('last 7 days');
    expect(windowSummary(99)).toBe('last year');
  });
});

describe('filters', () => {
  it('leaves empty values out of the request', () => {
    expect(healthQuery(filtersFrom({ hours: 24 }))).toEqual({ hours: 24 });
    expect(healthQuery({ project_id: null, task_id: 'task-1', hours: 168 })).toEqual({
      task_id: 'task-1',
      hours: 168
    });
  });

  it('leaves empty choices out of the URL', () => {
    expect(searchFrom({ task_id: '', provider: undefined, hours: 720 })).toEqual({ hours: 720 });
  });
});

describe('choices', () => {
  it('keeps a selected value visible under a fallback label when the list does not have it', () => {
    const options = [{ value: 'task-1', label: 'Add retry button' }];
    expect(withSelected(options, 'task-9', 'Selected task')).toEqual([
      ...options,
      { value: 'task-9', label: 'Selected task' }
    ]);
    expect(withSelected(options, 'task-1', 'Selected task')).toBe(options);
    expect(withSelected(options, '', 'Selected task')).toBe(options);
  });

  it('builds task, session and attempt options once each', () => {
    const rows = [
      row({ id: 'r1', task_id: 'task-1', title: 'Add retry button' }),
      row({ id: 'r2', task_id: 'task-1', title: 'Add retry button' }),
      row({ id: 'r3', task_id: null, title: null, sessions: [session({ title: null })] })
    ];
    expect(taskChoices(rows)).toEqual([{ value: 'task-1', label: 'Add retry button' }]);
    // The first occurrence keeps its label, so the untitled repeat of attempt-1 adds nothing.
    expect(attemptChoices(rows)).toEqual([{ value: 'attempt-1', label: 'Retry work · attempt-' }]);
    expect(projectChoices([{ id: 'p1', name: 'Mainloop' } as never])).toEqual([
      { value: 'p1', label: 'Mainloop' }
    ]);
  });

  it('labels an untitled session by its id', () => {
    const rows = [
      row({
        sessions: [session({ session_id: 'session-7', title: null, attempt_id: 'attempt-9' })]
      })
    ];
    expect(attemptChoices(rows)).toEqual([{ value: 'attempt-9', label: 'session-7 · attempt-' }]);
    expect(sessionChoices(rows)).toEqual([{ value: 'session-7', label: 'session-7' }]);
  });
});

describe('observations', () => {
  it('drops an observation of an unknown kind and reads the rest', () => {
    const result = readObservations(
      events([toolEvent(), { event_id: 'x', payload: { kind: 'weird', sequence: 1, source: 's' } }])
    );
    expect(result?.events.map((event) => event.event_id)).toEqual(['event-1']);
    expect(result?.dropped).toBe(1);
  });

  it('returns null for a list that is not readable', () => {
    expect(readObservations({ events: 'nope', partial: false })).toBeNull();
  });

  it('shows a tool observation with its path, and leaves the path out when there is none', () => {
    expect(eventLines(readObservations(events([toolEvent()]))!.events[0]!)).toEqual([
      'tool · sequence 3 · claude_result_stream',
      'event-1',
      'shell · error · tool_error · src/app.ts'
    ]);
    const noPath = toolEvent({
      payload: {
        ...(toolEvent().payload as object),
        tool: { tool_class: 'mcp', outcome: 'success', category: 'none', path: null }
      }
    });
    expect(eventLines(readObservations(events([noPath]))!.events[0]!)[2]).toBe(
      'mcp · success · none'
    );
  });
});
