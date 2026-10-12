import { Link } from '@tanstack/react-router';
import { Button } from '@/components/ui/button';
import { agentLabel } from '@/lib/agentLabel';
import { agentKindOf, keepValid, repoKey, workspaceRowSchema, type WorkspaceRow } from './model';
import { useWorkspaces } from './queries';

const RECENT = 5;

/**
 * The repository's workspaces, with the requested branch first. Other branches of the repository
 * stay in the list, so a workspace that is still starting does not leave the owner with nothing to
 * check. The server sends newest first, and each group keeps that order.
 */
function recentFor(
  rows: WorkspaceRow[],
  repo: string | null | undefined,
  branch: string | undefined
): WorkspaceRow[] {
  const wanted = repo ? repoKey(repo) : null;
  const inRepo = rows.filter(
    (workspace) => wanted === null || repoKey(workspace.manifest.repo_url) === wanted
  );
  const onBranch = (workspace: WorkspaceRow) =>
    Boolean(branch) && workspace.manifest.branch === branch;
  return [...inRepo.filter(onBranch), ...inRepo.filter((workspace) => !onBranch(workspace))];
}

/**
 * Shown after a create whose reply was lost or failed with a 5xx. Mainloop may have started the
 * workspace anyway, so the owner sees the recent list, and the form stays locked until they confirm
 * they have looked. `reading` covers the create and the list re-read that follows it.
 */
export function CreateCheck({
  repo,
  branch,
  reading,
  onAcknowledge
}: {
  /** The repository the owner asked for. Without it, the list covers every repository. */
  repo?: string | null;
  /** The branch the owner asked for. Its workspaces are listed first. */
  branch?: string;
  reading: boolean;
  onAcknowledge: () => void;
}) {
  const list = useWorkspaces();
  const rows = recentFor(keepValid(list.data, workspaceRowSchema), repo, branch).slice(0, RECENT);
  // A list that failed to re-read cannot confirm anything, so the owner gets a retry, not a way on.
  const settled = !reading && list.isSuccess;

  return (
    <div
      data-testid="create-check"
      className="gap-sm p-md border-destructive text-foreground grid border text-sm"
    >
      <p role="alert" className="text-destructive font-medium">
        The workspace may have been created.
      </p>
      <p className="text-muted-foreground break-words">
        Mainloop did not confirm the new workspace, so one may already exist. Check the recent
        workspaces before creating another. A workspace that is still starting may not be listed
        yet.
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
                className="group gap-sm p-sm border-border bg-background text-foreground hover:bg-accent hover:text-accent-foreground flex min-h-11 flex-wrap items-center justify-between border"
              >
                <span className="break-words">
                  {workspace.manifest.repo_url.replace('https://github.com/', '')} ·{' '}
                  {workspace.manifest.branch}
                </span>
                <span className="text-muted-foreground group-hover:text-accent-foreground text-xs">
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
