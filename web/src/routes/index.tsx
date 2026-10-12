import { createFileRoute, useNavigate } from '@tanstack/react-router';
import { z } from 'zod';
import { ChatPage } from '@/features/chat/ChatPage';
import { ChatStarters } from '@/features/explorer';

export const Route = createFileRoute('/')({
  // `hitl` puts that approval card at the end of the thread. `prefill` fills the composer once,
  // unsent; it is cleared from the URL as soon as the composer has taken it.
  validateSearch: (search) =>
    z
      .object({
        hitl: z.string().optional().catch(undefined),
        prefill: z.string().optional().catch(undefined)
      })
      .parse(search),
  head: () => ({ meta: [{ title: 'Chat · Mainloop' }] }),
  component: MainThread
});

function MainThread() {
  const { hitl, prefill } = Route.useSearch();
  const navigate = useNavigate();
  return (
    <ChatPage
      hitlId={hitl}
      prefill={prefill}
      starters={<ChatStarters />}
      onPrefillUsed={() =>
        void navigate({
          to: '/',
          search: (prev) => ({ ...prev, prefill: undefined }),
          replace: true
        })
      }
    />
  );
}
