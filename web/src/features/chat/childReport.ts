/** A child agent's report arrives in the main thread as a user-role message with this header. */
const REPORT = /^\[report from child ([0-9a-f]{8}) '([^']*)'([^\]]*)\]\n?/;

export interface ChildReport {
  childId: string;
  title: string;
  /** The child ended without writing a report; the text is the harness's fallback. */
  fallback: boolean;
  body: string;
}

/** The report in a message's text, or null when it is something the owner wrote. */
export function parseChildReport(content: string): ChildReport | null {
  const match = REPORT.exec(content);
  if (!match) return null;
  const [header, childId = '', title = '', flags = ''] = match;
  return {
    childId,
    title,
    fallback: flags.includes('fallback'),
    body: content.slice(header.length)
  };
}
