import { Link } from '@tanstack/react-router';
import { Button } from '@/components/ui/button';
import { agentLabel } from '@/lib/agentLabel';
import { agentKindOf, keepValid, workspaceRowSchema, type WorkspaceLifecycle } from './model';
import { useWorkspaces } from './queries';

const RECENT = 5;

/**
 * Shown after a create whose reply was lost or failed with a 5xx. Mainloop may have started the
 * workspace anyway, so the owner sees the recent list, and the form stays locked until they confirm
 * they have looked. `reading` covers the create and the list re-read that follows it.
 */
export function CreateCheck({
  repoUrl,
  reading,
  onAcknowledge
}: {
  /** List only workspaces of this repository. Without it, the list covers every repository. */
  repoUrl?: string;
  reading: boolean;
  onAcknowledge: () => void;
}) {
  const list = useWorkspaces();
  const rows = keepValid<WorkspaceLifecycle>(list.data, workspaceRowSchema)
    .filter((workspace) => !repoUrl || workspace.manifest.repo_url === repoUrl)
    .slice(0, RECENT);
  // A list that failed to re-read cannot confirm anything, so the owner gets a retry, not a way on.
  const settled = !reading && list.isSuccess && !list.isFetching;

  return (
    <div
      role="alert"
      data-testid="create-check"
      className="gap-sm p-md border-destructive text-foreground grid border text-sm"
    >
      <p className="text-destructive font-medium">The workspace may have been created.</p>
      <p className="text-muted-foreground break-words">
        Mainloop did not confirm the new workspace, so one may already exist. Check the recent
        workspaces before creating another.
      </p>

      {list.isError ? (
        <div className="gap-sm flex flex-col items-start">
          <p className="text-muted-foreground">Couldn't read the workspace list.</p>
          <Button
            type="button"
            variant="outline"
            onClick={() => void list.refetch()}
            disabled={list.isFetching}
          >
            Retry loading workspaces
          </Button>
        </div>
      ) : null}

      {!settled && !list.isError ? (
        <p role="status" className="text-muted-foreground">
          Checking the recent workspaces…
        </p>
      ) : null}

      {settled && rows.length === 0 ? (
        <p className="text-muted-foreground">No recent workspaces are listed.</p>
      ) : null}

      {settled && rows.length > 0 ? (
        <ul className="gap-xs flex flex-col">
          {rows.map((workspace) => (
            <li key={workspace.workspace_id}>
              <Link
                to="/workspaces/$id"
                params={{ id: workspace.workspace_id }}
                className="gap-sm p-sm border-border bg-background text-foreground hover:bg-accent flex min-h-11 flex-wrap items-center justify-between border"
              >
                <span className="break-words">
                  {workspace.manifest.repo_url.replace('https://github.com/', '')} ·{' '}
                  {workspace.manifest.branch}
                </span>
                <span className="text-muted-foreground text-xs">
                  {agentLabel(agentKindOf(workspace.manifest.agent_kind))}
                </span>
              </Link>
            </li>
          ))}
        </ul>
      ) : null}

      <div>
        <Button type="button" variant="outline" onClick={onAcknowledge} disabled={!settled}>
          I've checked the list
        </Button>
      </div>
    </div>
  );
}
