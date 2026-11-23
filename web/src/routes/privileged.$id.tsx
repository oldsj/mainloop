import { createFileRoute } from '@tanstack/react-router';
import { PrivilegedActionPage } from '@/features/privileged';

export const Route = createFileRoute('/privileged/$id')({
  head: () => ({ meta: [{ title: 'Privileged action · Mainloop' }] }),
  component: PrivilegedAction
});

function PrivilegedAction() {
  const { id } = Route.useParams();
  return <PrivilegedActionPage key={id} id={id} />;
}
