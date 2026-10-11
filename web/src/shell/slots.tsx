import { InboxPanel as InboxFeature } from '@/features/inbox';
import { SessionsPanel as SessionsFeature } from '@/features/sessions';
import { RoutePlaceholder } from './RoutePlaceholder';

/**
 * Panels the shells host. Feature slices own their bodies: sessions (F4) and inbox (F5). Projects
 * (F7) is still a placeholder. The shell only decides where they sit. The slot wrappers carry the
 * test ids, and `contents` keeps the feature's own layout as it was.
 */
export function SessionsPanel() {
  return (
    <div data-testid="sessions-panel" className="contents">
      <SessionsFeature />
    </div>
  );
}

export function InboxPanel() {
  return (
    <div data-testid="inbox-panel" className="contents">
      <InboxFeature />
    </div>
  );
}

export function ProjectsPanel() {
  return <RoutePlaceholder title="Projects" slice="F7" testId="projects-panel" />;
}
