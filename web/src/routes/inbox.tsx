import { createFileRoute, Link } from '@tanstack/react-router';
import { ChevronLeft } from 'lucide-react';
import { z } from 'zod';
import { buttonVariants } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';
import { InboxPanel } from '@/features/inbox/InboxPanel';

// `show` picks Needs you, Problems or Activity; `item` names the Needs-you row to bring into view,
// by its approval request id or queue item id (what the explorer and a task's pending approvals
// carry).
export const Route = createFileRoute('/inbox')({
  validateSearch: (search) =>
    z
      .object({
        show: z.enum(['needs', 'problems', 'activity']).optional().catch(undefined),
        item: z.string().optional().catch(undefined)
      })
      .parse(search),
  head: () => ({ meta: [{ title: 'Inbox · Mainloop' }] }),
  component: Inbox
});

/** The answer view, in place of the chat. Its back link returns to the chat. */
function Inbox() {
  const { show = 'needs', item } = Route.useSearch();
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
          search={{}}
          className={cn(buttonVariants({ variant: 'ghost', size: 'icon' }))}
          aria-label="Back to chat"
          title="Back to cha"
        >
          <ChevronLeft aria-hidden="true" />
        </Link>
      }
    />
  );
}
