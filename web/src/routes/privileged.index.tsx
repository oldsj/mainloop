import { createFileRoute } from '@tanstack/react-router';
import { PrivilegedAuditPage } from '@/features/privileged';

export const Route = createFileRoute('/privileged/')({
  head: () => ({ meta: [{ title: 'Privileged actions · Mainloop' }] }),
  component: PrivilegedAuditPage
});
