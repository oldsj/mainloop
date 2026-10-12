import { createFileRoute, Link, useSearch } from '@tanstack/react-router';
import { ChevronLeft } from 'lucide-react';
import { z } from 'zod';
import { buttonVariants } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';
import { InboxPanel } from '@/features/inbox/InboxPanel';
import { useShellView } from '@/shell/view';

// `show` picks Needs you or Activity; `item` names the Needs-you row to bring into view, by its
// approval request id or queue item id (what the explorer and a task's pending approvals carry).
export const Route = createFileRoute('/inbox')({
  validateSearch: (search) =>
    z
      .object({
        show: z.enum(['needs', 'activity']).optional().catch(undefined),
        item: z.string().optional().catch(undefined)
      })
      .parse(search),
  head: () => ({ meta: [{ title: 'Inbox · Mainloop' }] }),
  component: Inbox
});

/**
 * The answer view, in place of the chat. Its back link returns to where the owner came from: the
 * Explorer tab on the phone, the chat on desktop (where the explorer stays beside it).
 */
function Inbox() {
  const { show = 'needs', item } = Route.useSearch();
  const { view } = useSearch({ strict: false });
  const compact = useShellView(view) === 'compact';
  const navigate = Route.useNavigate();
  return (
    <InboxPanel
      view={show}
      selected={item}
      onView={(next) =>
        void navigate({
          search: (prev) => ({
            ...prev,
            show: next === 'needs' ? undefined : next,
            item: undefined
          })
        })
      }
      back={
        <Link
          to="/"
          search={compact ? { tab: 'explorer' } : {}}
          className={cn(buttonVariants({ variant: 'ghost', size: 'icon' }))}
          aria-label={compact ? 'Back to Explorer' : 'Back to chat'}
          title={compact ? 'Back to Explorer' : 'Back to chat'}
        >
          <ChevronLeft aria-hidden="true" />
        </Link>
      }
    />
  );
}
