import { createFileRoute } from '@tanstack/react-router';
import { SessionPage } from '@/features/sessions';

export const Route = createFileRoute('/sessions/$id')({
  component: Session
});

function Session() {
  const { id } = Route.useParams();
  return <SessionPage id={id} />;
}
