import { RoutePlaceholder } from './RoutePlaceholder';

/**
 * Panels the shells host. Feature slices replace the bodies: sessions (F4), inbox (F5) and
 * projects (F7). The shell only decides where they sit.
 */
export function SessionsPanel() {
  return <RoutePlaceholder title="Sessions" slice="F4" testId="sessions-panel" />;
}

export function InboxPanel() {
  return <RoutePlaceholder title="Inbox" slice="F5" testId="inbox-panel" />;
}

export function ProjectsPanel() {
  return <RoutePlaceholder title="Projects" slice="F7" testId="projects-panel" />;
}
