import { createFileRoute } from '@tanstack/react-router';
import { SessionPage } from '@/features/sessions';

export const Route = createFileRoute('/sessions/$id')({
  head: () => ({ meta: [{ title: 'Session · Mainloop' }] }),
  component: Session
});

function Session() {
  const { id } = Route.useParams();
  // Keyed so that moving to another session remounts the page, and its notice and pending actions do not carry over.
  return <SessionPage key={id} id={id} />;
}
