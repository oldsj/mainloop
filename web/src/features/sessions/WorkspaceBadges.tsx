import { cn } from '@/components/ui/cn';
import { StatusBadge } from '@/components/shared/status-badge';
import type { WorkspaceBadgeFields } from './model';
import { publicationText, workspaceTone } from './model';

/**
 * The workspace's observed state as a status word, with the backend's detail on hover. `className`
 * lets a filled row override the tone colour.
 */
export function WorkspaceLifecycleBadge({
  workspace,
  className
}: {
  workspace: WorkspaceBadgeFields;
  className?: string;
}) {
  const state = workspace.observed_state;
  return (
    <span data-testid="workspace-lifecycle-badge" title={workspace.detail ?? `Workspace ${state}.`}>
      <StatusBadge
        label={`WS ${state.toUpperCase()}`}
        tone={workspaceTone(state)}
        className={className}
      />
    </span>
  );
}

/** Whether the workspace may publish, and why not when it may not. */
export function WorkspacePublicationBadge({
  workspace,
  className
}: {
  workspace: WorkspaceBadgeFields;
  className?: string;
}) {
  return (
    <span
      data-testid="workspace-publication-badge"
      title={workspace.publication_reason ?? 'Branch publication granted'}
      className={cn(
        'px-sm py-xs text-muted-foreground border-border inline-flex max-w-full border text-xs',
        className
      )}
    >
      {publicationText(workspace)}
    </span>
  );
}
