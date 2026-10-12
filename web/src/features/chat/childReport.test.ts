import { describe, expect, it } from 'vitest';
// The exact texts the Go server delivers; a Go test checks `reportText` against the same file.
import fixture from './testdata/report_texts.json';
import { parseChildReport } from './childReport';

interface ReportText {
  name: string;
  task_id: string;
  child: boolean;
  outcome: string;
  summary: string;
  owner_action: string;
  text: string;
}

const texts: ReportText[] = fixture;

describe('parseChildReport', () => {
  it('has the Go server texts to read', () => {
    expect(texts.length).toBeGreaterThan(0);
  });

  it.each(texts.map((t) => [t.name, t] as const))(
    'reads the task, outcome and body of %s',
    (_name, t) => {
      const report = parseChildReport(t.text);
      expect(report).toEqual({
        taskId: t.task_id,
        title: `${t.child ? 'child task' : 'task'} ${t.task_id.slice(0, 8)}`,
        outcome: t.outcome,
        body: t.owner_action ? `${t.summary}\nOwner action needed: ${t.owner_action}` : t.summary
      });
    }
  );

  it.each([
    ['plain text', 'hello'],
    [
      'a header that is not first',
      'text [report from child task 0a1b2c3d-1 attempt 1; completed; unverified claim]\nbody'
    ],
    ['the frozen backend’s header', "[report from child 0a1b2c3d 'Fix the build']\nbody"],
    ['an uppercase task id', '[report from task 0A1B2C3D attempt 1; failed; unverified claim]\nb']
  ])('returns null for %s', (_name, content) => {
    expect(parseChildReport(content)).toBeNull();
  });
});
