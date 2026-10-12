/**
 * A task's report arrives in a conversation as a user-role message whose first line is the header
 * the Go server writes (`reportText` in server/internal/tasks/deliver.go):
 * `[report from child task <id> attempt <n>; <outcome>; unverified claim]`, or `report from task`
 * for a top-level task. server/internal/tasks/testdata/report_texts.json holds the exact texts and
 * both sides test against it.
 */
const REPORT =
  /^\[report from (child task|task) ([0-9a-f-]+) attempt (\d+); ([a-z_]+); [^\]]*\]\n?/;

export interface ChildReport {
  taskId: string;
  /** The task as the owner sees it: its kind and short id, e.g. `child task 0a1b2c3d`. */
  title: string;
  outcome: string;
  body: string;
}

/** The report in a message's text, or null when it is something the owner wrote. */
export function parseChildReport(content: string): ChildReport | null {
  const match = REPORT.exec(content);
  if (!match) return null;
  const [header, kind = '', taskId = '', , outcome = ''] = match;
  return {
    taskId,
    title: `${kind} ${taskId.slice(0, 8)}`,
    outcome,
    body: content.slice(header.length)
  };
}
