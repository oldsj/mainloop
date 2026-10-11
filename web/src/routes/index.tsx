import { createFileRoute } from '@tanstack/react-router';
import { z } from 'zod';
import { ChatPage } from '@/features/chat/ChatPage';

export const Route = createFileRoute('/')({
  // `hitl` puts that approval card at the top of the thread.
  validateSearch: (search) =>
    z.object({ hitl: z.string().optional().catch(undefined) }).parse(search),
  component: MainThread
});

function MainThread() {
  const { hitl } = Route.useSearch();
  return <ChatPage hitlId={hitl} />;
}
