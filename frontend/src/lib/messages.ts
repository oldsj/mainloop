/** Helpers for showing the native main thread's conversation. */
import type { Message } from '$lib/api';

const PRE_CUT = '[mainloop:pre-cut]';
const REPORT = /^\[report from child ([0-9a-f]{8}) '([^']*)'([^\]]*)\]\n?/;

export interface ChildReport {
  childId: string;
  title: string;
  fallback: boolean;
  body: string;
}

/** A child's report delivered to the main thread as a message, or null. */
export function parseChildReport(content: string): ChildReport | null {
  const m = REPORT.exec(content);
  if (!m) return null;
  return {
    childId: m[1],
    title: m[2],
    fallback: m[3].includes('fallback'),
    body: content.slice(m[0].length)
  };
}

/**
 * Hide protocol traffic: the pre-cut write-out prompt and the agent's reply to it. Both stay in
 * Postgres; they just are not a conversation the user had.
 */
export function visibleMessages(messages: Message[]): Message[] {
  const out: Message[] = [];
  let skipReply = false;
  for (const m of messages) {
    if (m.role === 'user' && m.content.startsWith(PRE_CUT)) {
      skipReply = true;
      continue;
    }
    if (skipReply && m.role === 'assistant') {
      skipReply = false;
      continue;
    }
    skipReply = false;
    out.push(m);
  }
  return out;
}
