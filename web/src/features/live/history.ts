import type { Entry } from './store';

/**
 * The owner's sent messages in this session, oldest first, for recall in the composer: every user
 * message the runner accepted (the attach backlog and the ones sent since). A message repeated back
 * to back is kept once, as a shell history does.
 */
export function sentHistory(entries: readonly Entry[]): string[] {
  const out: string[] = [];
  for (const e of entries) {
    if (e.t !== 'user') continue;
    const text = e.text.replace(/ {2}\(cancelled\)$/, '');
    if (!text.trim() || out[out.length - 1] === text) continue;
    out.push(text);
  }
  return out;
}

/** Whether the caret sits on the first line, with nothing selected: Up then recalls instead of moving. */
export function caretOnFirstLine(value: string, start: number, end: number): boolean {
  return start === end && !value.slice(0, start).includes('\n');
}

/** Whether the caret sits on the last line, with nothing selected: Down then recalls instead of moving. */
export function caretOnLastLine(value: string, start: number, end: number): boolean {
  return start === end && !value.slice(end).includes('\n');
}

/**
 * One step through history. `at` is the recalled index, or null while editing the draft. Returns
 * the new index (null = back at the draft), or undefined when there is nowhere to go.
 */
export function step(
  at: number | null,
  length: number,
  dir: 'up' | 'down'
): number | null | undefined {
  if (dir === 'up') {
    if (length === 0) return undefined;
    if (at === null) return length - 1;
    return at > 0 ? at - 1 : undefined;
  }
  if (at === null) return undefined;
  return at < length - 1 ? at + 1 : null;
}
