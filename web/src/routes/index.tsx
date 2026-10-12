import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';
import { ChatPage } from '@/features/chat/ChatPage';
import { HomeWork } from '@/features/home';

export const Route = createFileRoute('/')({
  // `hitl` puts that approval card at the top of the thread.
  validateSearch: (search) =>
    z.object({ hitl: z.string().optional().catch(undefined) }).parse(search),
  head: () => ({ meta: [{ title: 'Chat · Mainloop' }] }),
  component: MainThread
});

function MainThread() {
  const { hitl } = Route.useSearch();
  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="max-h-[45vh] shrink-0 overflow-y-auto">
        <HomeWork />
      </div>
      <div className="min-h-0 flex-1">
        <ChatPage hitlId={hitl} />
      </div>
    </div>
  );
}
