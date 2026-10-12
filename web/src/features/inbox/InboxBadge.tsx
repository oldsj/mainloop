import { badgeText, unresolvedCount } from './model';
import { useAttentionPoll } from './poll';
import { useAttention } from './queries';

/**
 * The unresolved count for the Needs-you tab, capped at 99+. Activity never adds to it. It polls
 * for itself, so it stays fresh where the inbox panel is not mounted.
 */
export function InboxBadge() {
  const attention = useAttention();
  useAttentionPoll();
  const text = badgeText(unresolvedCount(attention.data));
  if (!text) return null;

  return (
    <span
      data-testid="inbox-badge"
      className="bg-primary text-primary-foreground inline-flex min-w-5 items-center justify-center px-1 text-xs tabular-nums"
    >
      <span className="sr-only">Needs you: </span>
      {text}
    </span>
  );
}
