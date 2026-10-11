import { useState } from 'react';
import { Button } from '@/components/ui/button';
import { ActivityList } from './ActivityList';
import { AttentionList } from './AttentionList';
import { badgeText, unresolvedCount } from './model';
import { useAttentionPoll } from './poll';
import { useAttention } from './queries';

type View = 'attention' | 'activity';

/**
 * The inbox: Needs you (the owner's open actions) and Activity (the recent log). Attention is
 * polled while this panel is mounted, and SSE `inbox:updated` refreshes it sooner.
 */
export function InboxPanel() {
  const [view, setView] = useState<View>('attention');
  const attention = useAttention();
  useAttentionPoll();
  const count = badgeText(unresolvedCount(attention.data));

  return (
    <section aria-labelledby="inbox-heading" className="flex flex-col">
      <div className="gap-sm p-md flex flex-wrap items-center justify-between">
        <h2 id="inbox-heading" className="text-sm font-medium">
          Inbox
        </h2>
        <div role="group" aria-label="Inbox views" className="gap-xs flex items-center">
          <Button
            variant={view === 'attention' ? 'outline' : 'ghost'}
            size="sm"
            aria-pressed={view === 'attention'}
            onClick={() => setView('attention')}
          >
            Needs you
            {count ? <span className="ml-xs tabular-nums">{count}</span> : null}
          </Button>
          <Button
            variant={view === 'activity' ? 'outline' : 'ghost'}
            size="sm"
            aria-pressed={view === 'activity'}
            onClick={() => setView('activity')}
          >
            Activity
          </Button>
        </div>
      </div>

      {view === 'attention' ? <AttentionList /> : <ActivityList />}
    </section>
  );
}
