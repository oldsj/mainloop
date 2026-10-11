import { StatusBadge } from '@/components/shared/status-badge';
import type { WorkspaceLifecycle } from './model';
import { publicationText, workspaceTone } from './model';

/** The workspace's observed state as a status word, with the backend's detail on hover. */
export function WorkspaceLifecycleBadge({ workspace }: { workspace: WorkspaceLifecycle }) {
  const state = workspace.observed_state;
  return (
    <span data-testid="workspace-lifecycle-badge" title={workspace.detail ?? `Workspace ${state}.`}>
      <StatusBadge label={`WS ${state.toUpperCase()}`} tone={workspaceTone(state)} />
    </span>
  );
}

/** Whether the workspace may publish, and why not when it may not. */
export function WorkspacePublicationBadge({ workspace }: { workspace: WorkspaceLifecycle }) {
  return (
    <span
      data-testid="workspace-publication-badge"
      title={workspace.publication_reason ?? 'Branch publication granted'}
      className="px-sm py-xs text-muted-foreground border-border inline-flex max-w-full border text-xs"
    >
      {publicationText(workspace)}
    </span>
  );
}
