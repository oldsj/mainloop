import { HomeStrip, WorkList } from '@/features/home';
import { InboxPanel as InboxFeature } from '@/features/inbox';
import { ProjectsPanel as ProjectsFeature } from '@/features/projects';
import { SessionsPanel as SessionsFeature } from '@/features/sessions';

/**
 * Panels the shells host. Feature slices own their bodies: home's work, sessions (F4),
 * inbox (F5) and projects (F7). The shell only decides where they sit. The slot wrappers are plain
 * blocks, not `display: contents`, so the desktop shell's dividers draw between panels. The work,
 * sessions and inbox wrappers carry a test id that the shell tests use; the projects wrapper does
 * not, and its section's own `projects-panel` id is what those tests find.
 */
export function WorkPanel({
  lastVisit,
  strip = false
}: {
  lastVisit: number | null;
  strip?: boolean;
}) {
  return (
    <div data-testid="work-panel">
      {strip ? <HomeStrip lastVisit={lastVisit} /> : null}
      <WorkList lastVisit={lastVisit} />
    </div>
  );
}

export function SessionsPanel() {
  return (
    <div data-testid="sessions-panel">
      <SessionsFeature />
    </div>
  );
}

export function InboxPanel() {
  return (
    <div data-testid="inbox-panel">
      <InboxFeature />
    </div>
  );
}

export function ProjectsPanel() {
  return (
    <div>
      <ProjectsFeature />
    </div>
  );
}
