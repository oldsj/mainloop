/** Time of a chat message: just the time today, with the date once it is from an earlier day. */
export function messageTime(iso: string, now: Date = new Date()): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return '';
  const time = date.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
  if (date.toDateString() === now.toDateString()) return time;
  const sameYear = date.getFullYear() === now.getFullYear();
  const day = date.toLocaleDateString([], {
    month: 'short',
    day: 'numeric',
    ...(sameYear ? {} : { year: 'numeric' })
  });
  return `${day}, ${time}`;
}

/** Time since `startedAt` (a UTC ISO-8601 stamp, with or without an offset): `7s`, `2m 05s`. */
export function elapsedSince(startedAt: string, now: number = Date.now()): string {
  const hasZone = /(?:Z|[+-]\d\d:?\d\d)$/.test(startedAt);
  const start = Date.parse(hasZone ? startedAt : `${startedAt}Z`);
  if (Number.isNaN(start)) return '';
  const seconds = Math.max(0, Math.floor((now - start) / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  const rest = String(seconds % 60).padStart(2, '0');
  return minutes < 60
    ? `${minutes}m ${rest}s`
    : `${Math.floor(minutes / 60)}h ${String(minutes % 60).padStart(2, '0')}m`;
}
