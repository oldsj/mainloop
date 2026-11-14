import { createFileRoute } from '@tanstack/react-router';
import { SettingsPage } from '@/features/settings';

export const Route = createFileRoute('/settings')({
  head: () => ({ meta: [{ title: 'Settings · Mainloop' }] }),
  component: SettingsPage
});
