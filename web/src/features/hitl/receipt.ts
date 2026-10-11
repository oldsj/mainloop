import { parseHITL, requestTools } from '@/lib/hitl';
import type { HITLView } from '@/lib/contract';
import { messageTime } from '@/lib/time';

/**
 * The one-line receipt of a recorded response, such as "Approved Bash, rejected Edit · 6:20 PM".
 * Tool names come from the request, so the owner reads names, not call ids. The time is when this
 * browser sent the response; the server's receipt carries none. Empty when nothing was recorded.
 */
export function receiptLine(view: HITLView, answeredAt: string | null): string {
  const response = view.response?.response;
  if (!response) return '';
  const payload = parseHITL(view.request.payload);
  const names = new Map(
    payload?.type === 'tool_approval_request'
      ? requestTools(payload).map((tool) => [tool.id, tool.name])
      : []
  );
  const summary =
    response.type === 'tool_approval_response'
      ? response.approvals
          .map((a) => `${a.approved ? 'approved' : 'rejected'} ${names.get(a.id) ?? a.id}`)
          .join(', ')
      : `answered ${response.answers.length} ${response.answers.length === 1 ? 'question' : 'questions'}`;
  const headline = summary
    ? summary.charAt(0).toUpperCase() + summary.slice(1)
    : 'Response recorded';
  const time = answeredAt ? messageTime(answeredAt) : '';
  return time ? `${headline} · ${time}` : headline;
}
