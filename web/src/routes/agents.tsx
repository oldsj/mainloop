import { createFileRoute } from '@tanstack/react-router';
import { NewAgentPage } from '@/features/sessions';

export const Route = createFileRoute('/agents')({
  head: () => ({ meta: [{ title: 'New agent session · Mainloop' }] }),
  component: NewAgentPage
});
