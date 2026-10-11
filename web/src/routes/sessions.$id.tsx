import { createFileRoute } from '@tanstack/react-router';
import { RoutePlaceholder } from '@/shell/RoutePlaceholder';

export const Route = createFileRoute('/sessions/$id')({
  component: Session
});

function Session() {
  const { id } = Route.useParams();
  return <RoutePlaceholder title="Session" slice="F4" detail={`${id}: not built yet (F4).`} />;
}
