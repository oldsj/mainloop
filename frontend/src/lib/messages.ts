/** Helpers for showing the native main thread's conversation. */
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
