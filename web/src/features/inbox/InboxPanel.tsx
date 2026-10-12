import { useState, type ReactNode } from 'react';
import { Button } from '@/components/ui/button';
import { ActivityList } from './ActivityList';
import { AttentionList } from './AttentionList';
import { badgeText, unresolvedCount } from './model';
import { useAttentionPoll } from './poll';
import { useAttention } from './queries';

export type InboxView = 'needs' | 'activity';

/**
 * The inbox: Needs you (the owner's open actions) and Activity (the recent log). Attention is
 * polled while this panel is mounted, and SSE `inbox:updated` refreshes it sooner. The `/inbox`
 * route drives `view` from the URL; without `onView` the panel keeps the view itself. `selected`
 * names the Needs-you row to bring into view, and `back` sits before the heading.
 */
export function InboxPanel({
  view: shown,
  onView,
  selected,
  back
}: {
  view?: InboxView;
  onView?: (view: InboxView) => void;
  selected?: string;
  back?: ReactNode;
}) {
  const [own, setOwn] = useState<InboxView>('needs');
  const view = shown ?? own;
  const show = onView ?? setOwn;
  const attention = useAttention();
  useAttentionPoll();
  const count = badgeText(unresolvedCount(attention.data));

  return (
    <section aria-labelledby="inbox-heading" className="flex flex-col">
      <div className="gap-sm p-md flex flex-wrap items-center justify-between">
        <div className="gap-xs flex items-center">
          {back}
          <h2 id="inbox-heading" className="text-sm font-medium">
            Inbox
          </h2>
        </div>
        <div role="group" aria-label="Inbox views" className="gap-xs flex items-center">
          <Button
            variant={view === 'needs' ? 'outline' : 'ghost'}
            size="sm"
            aria-pressed={view === 'needs'}
            onClick={() => show('needs')}
          >
            Needs you
            {count ? <span className="ml-xs tabular-nums">{count}</span> : null}
          </Button>
          <Button
            variant={view === 'activity' ? 'outline' : 'ghost'}
            size="sm"
            aria-pressed={view === 'activity'}
            onClick={() => show('activity')}
          >
            Activity
          </Button>
        </div>
      </div>

      {view === 'needs' ? <AttentionList selected={selected} /> : <ActivityList />}
    </section>
  );
}
