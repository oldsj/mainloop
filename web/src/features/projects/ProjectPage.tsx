import { Link, useNavigate } from '@tanstack/react-router';
import type { ReactNode } from 'react';
import { useState } from 'react';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { StatusBadge } from '@/components/shared/status-badge';
import { Button } from '@/components/ui/button';
import { EnvironmentSection } from '@/features/environments/EnvironmentSection';
import { SessionRow } from '@/features/sessions/SessionRow';
import { relativeTime, workspaceFor } from '@/features/sessions/model';
import {
  WorkspaceLifecycleBadge,
  WorkspacePublicationBadge
} from '@/features/sessions/WorkspaceBadges';
import { agentLabel } from '@/lib/agentLabel';
import type { ProviderProfile, Session } from '@/lib/contract';
import { safeExternalUrl } from '@/lib/taskState';
import { useConnection } from '@/shell/ConnectionBanner';
import { AgentRadios } from './AgentRadios';
import { CreateCheck } from './CreateCheck';
import { MergePolicySection } from './MergePolicySection';
import {
  agentKindOf,
  commitRowSchema,
  commitTitle,
  failureText,
  keepValid,
  openPrText,
  projectRowSchema,
  providerRowSchema,
  pullRequestRowSchema,
  sessionRowSchema,
  workspaceRequest,
  workspaceRowSchema,
  type AgentKind,
  type Commit,
  type PullRequest,
  type WorkspaceLifecycle
} from './model';
import { useCreateWorkspace, useProjectDetail, useProviders, useWorkspaces } from './queries';

export interface ProjectPageProps {
  id: string;
  /**
   * The project's task tree (F6). Until F6 is wired in, the section shows its empty note. The
   * supervisor passes the tree here once both slices have merged.
   */
  tasks?: ReactNode;
}

/**
 * One project: its merge policy, environment, open pull requests, branch workspaces, recent commits
 * and sessions. The route keys it by id, so a different project starts on a fresh page.
 */
export function ProjectPage({ id, tasks }: ProjectPageProps) {
  const detail = useProjectDetail(id);
  const workspaces = useWorkspaces();
  const providers = useProviders();
  const create = useCreateWorkspace(id);
  const uncertain = create.uncertain;
  const navigate = useNavigate();

  const data = detail.data;
  if (!data) {
    // A retry shows the loading line again, so the owner sees that the request is running.
    if (detail.isError && !detail.isFetching) {
      return (
        <ErrorRecovery
          title="Couldn't load the project"
          message={failureText(detail.error)}
          onRetry={() => void detail.refetch()}
          retryLabel="Retry loading project"
        />
      );
    }
    return (
      <p role="status" className="text-muted-foreground p-md text-sm">
        Loading project...
      </p>
    );
  }

  const project = projectRowSchema.safeParse(data.project).success ? data.project : null;
  if (!project) {
    return (
      <ErrorRecovery
        title="Couldn't load the project"
        message="Mainloop sent a project that does not match the contract."
      />
    );
  }

  const avatar = safeExternalUrl(project.avatar_url);
  const github = safeExternalUrl(project.html_url);
  const pulls = keepValid<PullRequest>(data.open_prs, pullRequestRowSchema);
  const commits = keepValid<Commit>(data.recent_commits, commitRowSchema);
  const sessions = keepValid<Session>(data.sessions, sessionRowSchema);
  const providerRows = keepValid<ProviderProfile>(providers.data, providerRowSchema);
  const workspaceRows = keepValid<WorkspaceLifecycle>(workspaces.data, workspaceRowSchema);
  const branchWorkspaces = workspaceRows.filter(
    (workspace) => workspace.manifest.repo_url === project.html_url
  );

  const startWorkspace = (target: { branch: string; agentKind: AgentKind }) => {
    if (create.pending || uncertain) return;
    create.mutate(
      { body: workspaceRequest({ projectId: id, ...target }) },
      {
        onSuccess: (workspace) =>
          void navigate({ to: '/workspaces/$id', params: { id: workspace.workspace_id } })
      }
    );
  };

  return (
    <article
      aria-labelledby="project-heading"
      className="gap-lg p-md mx-auto flex w-full max-w-3xl min-w-0 flex-col"
    >
      <title>{`${project.full_name} - mainloop`}</title>
      {detail.isError ? (
        <div data-testid="project-refresh-error">
          <ErrorRecovery
            title="Couldn't refresh the project"
            message={`${failureText(detail.error)} Showing the last loaded project.`}
            onRetry={() => void detail.refetch()}
            retryLabel="Retry loading project"
            pending={detail.isFetching}
          />
        </div>
      ) : null}

      <header className="gap-md flex items-start">
        {avatar ? <img src={avatar} alt="" className="size-12 shrink-0" /> : null}
        <div className="gap-xs flex min-w-0 flex-col">
          <h1 id="project-heading" className="text-lg font-medium break-words">
            {project.full_name}
          </h1>
          {project.description ? (
            <p className="text-muted-foreground text-sm break-words">{project.description}</p>
          ) : null}
          <div className="gap-sm text-muted-foreground flex flex-wrap items-center text-xs">
            {github ? (
              <a
                href={github}
                target="_blank"
                rel="noopener noreferrer"
                className="text-foreground inline-flex min-h-11 items-center underline"
              >
                View on GitHub →
              </a>
            ) : null}
            <span>{openPrText(project.open_pr_count)}</span>
            <Link
              to="/health"
              search={{ project_id: id }}
              className="text-foreground inline-flex min-h-11 items-center underline"
            >
              Orchestration health
            </Link>
          </div>
        </div>
      </header>

      <MergePolicySection projectId={id} />
      <EnvironmentSection projectId={id} />

      <section
        aria-labelledby="project-tasks-heading"
        className="gap-md flex flex-col"
        data-testid="project-tasks"
      >
        <h2 id="project-tasks-heading" className="text-lg font-medium">
          Tasks
        </h2>
        {tasks ?? <p className="text-muted-foreground text-sm">No tasks for this project yet.</p>}
      </section>

      <section aria-labelledby="project-prs-heading" className="gap-md flex flex-col">
        <h2 id="project-prs-heading" className="text-lg font-medium">
          Open Pull Requests
        </h2>
        {pulls.length === 0 ? (
          <p className="text-muted-foreground text-sm">No open pull requests</p>
        ) : (
          <ul className="gap-sm flex flex-col">
            {pulls.map((pr) => (
              <PullRequestLine key={pr.number} pr={pr} />
            ))}
          </ul>
        )}
      </section>

      <section aria-labelledby="project-workspaces-heading" className="gap-md flex flex-col">
        <h2 id="project-workspaces-heading" className="text-lg font-medium">
          Branch workspaces
        </h2>
        <BranchWorkspaceForm
          defaultBranch={project.default_branch}
          pending={create.pending}
          locked={uncertain !== null}
          onSubmit={startWorkspace}
        />
        {uncertain ? (
          <CreateCheck
            repo={project.html_url}
            branch={uncertain.branch}
            reading={create.pending}
            onAcknowledge={create.acknowledge}
          />
        ) : create.error ? (
          <div role="alert" className="text-destructive text-sm">
            {failureText(create.error)}
          </div>
        ) : null}

        {workspaces.isError && !workspaces.data ? (
          <ErrorRecovery
            title="Couldn't load branch workspaces"
            message={failureText(workspaces.error)}
            onRetry={() => void workspaces.refetch()}
            retryLabel="Retry loading workspaces"
            pending={workspaces.isFetching}
          />
        ) : null}
        {workspaces.isSuccess && branchWorkspaces.length === 0 ? (
          <p className="text-muted-foreground text-sm">No branch workspaces yet.</p>
        ) : null}
        {branchWorkspaces.length > 0 ? (
          <ul className="gap-sm flex flex-col">
            {branchWorkspaces.map((workspace) => (
              <li key={workspace.workspace_id}>
                <Link
                  to="/workspaces/$id"
                  params={{ id: workspace.workspace_id }}
                  className="gap-sm p-md border-border bg-background text-foreground hover:bg-accent flex min-h-11 flex-wrap items-center justify-between border"
                >
                  <span className="truncate text-sm font-medium">{workspace.manifest.branch}</span>
                  <span className="gap-sm flex flex-wrap items-center text-xs">
                    <span data-testid="workspace-agent-kind">
                      {agentLabel(agentKindOf(workspace.manifest.agent_kind))}
                    </span>
                    <WorkspaceLifecycleBadge workspace={workspace} />
                    <WorkspacePublicationBadge workspace={workspace} />
                    {workspace.last_activity_at ? (
                      <span className="text-muted-foreground">
                        Active {relativeTime(workspace.last_activity_at)}
                      </span>
                    ) : null}
                  </span>
                </Link>
              </li>
            ))}
          </ul>
        ) : null}
      </section>

      <section aria-labelledby="project-commits-heading" className="gap-md flex flex-col">
        <h2 id="project-commits-heading" className="text-lg font-medium">
          Recent Commits
        </h2>
        {commits.length === 0 ? (
          <p className="text-muted-foreground text-sm">No recent commits</p>
        ) : (
          <ul className="gap-sm flex flex-col">
            {commits.map((commit) => (
              <CommitLine key={commit.sha} commit={commit} />
            ))}
          </ul>
        )}
      </section>

      <section aria-labelledby="project-sessions-heading" className="gap-md flex flex-col">
        <h2 id="project-sessions-heading" className="text-lg font-medium">
          Sessions
        </h2>
        {sessions.length === 0 ? (
          <p className="text-muted-foreground text-sm">No sessions yet</p>
        ) : (
          <ul className="gap-sm flex flex-col">
            {sessions.map((session) => (
              <li key={session.id}>
                <SessionRow
                  session={session}
                  workspace={workspaceFor(workspaceRows, session.id)}
                  providers={providerRows}
                />
              </li>
            ))}
          </ul>
        )}
      </section>
    </article>
  );
}

function PullRequestLine({ pr }: { pr: PullRequest }) {
  const url = safeExternalUrl(pr.url);
  return (
    <li className="gap-xs flex flex-col text-sm">
      <div className="gap-sm flex flex-wrap items-center">
        {url ? (
          <a
            href={url}
            target="_blank"
            rel="noopener noreferrer"
            className="inline-flex min-h-11 min-w-11 items-center justify-center font-medium underline"
          >
            #{pr.number}
          </a>
        ) : (
          <span className="font-medium">#{pr.number}</span>
        )}
        {pr.is_mainloop ? <StatusBadge label="Mainloop" tone="active" /> : null}
        <span className="break-words">{pr.title}</span>
      </div>
      <span className="text-muted-foreground text-xs">
        by {pr.author} ·{' '}
        <time dateTime={pr.created_at}>{new Date(pr.created_at).toLocaleDateString()}</time>
      </span>
    </li>
  );
}

function CommitLine({ commit }: { commit: Commit }) {
  const url = safeExternalUrl(commit.url);
  return (
    <li className="gap-xs flex flex-col text-sm">
      <span className="break-words">{commitTitle(commit.message)}</span>
      <span className="text-muted-foreground gap-sm flex flex-wrap text-xs">
        <span>{commit.author}</span>
        <time dateTime={commit.date}>{new Date(commit.date).toLocaleDateString()}</time>
        {url ? (
          <a
            href={url}
            target="_blank"
            rel="noopener noreferrer"
            className="inline-flex min-h-11 min-w-11 items-center justify-center font-mono underline"
          >
            {commit.sha.slice(0, 7)}
          </a>
        ) : (
          <span className="font-mono">{commit.sha.slice(0, 7)}</span>
        )}
      </span>
    </li>
  );
}

/**
 * Starts a workspace on a branch of this project. The parent owns the request, so a failed create
 * stays in the section, and `pending` disables the button while the request is in flight.
 */
function BranchWorkspaceForm({
  defaultBranch,
  pending,
  locked,
  onSubmit
}: {
  defaultBranch: string;
  pending: boolean;
  /** An earlier create may have started a workspace; no new one until the owner has checked. */
  locked: boolean;
  onSubmit: (target: { branch: string; agentKind: AgentKind }) => void;
}) {
  const [branch, setBranch] = useState(defaultBranch);
  const [agentKind, setAgentKind] = useState<AgentKind>('claude');
  const offline = useConnection().status === 'offline';
  const blocked = offline || pending || locked;

  return (
    <form
      data-testid="project-workspace-form"
      className="gap-md p-md border-border flex flex-col border"
      onSubmit={(event) => {
        event.preventDefault();
        if (blocked) return;
        onSubmit({ branch: branch.trim() || defaultBranch, agentKind });
      }}
    >
      <label className="gap-xs flex flex-col text-sm">
        Branch
        <input
          aria-label="Workspace branch"
          className="border-border bg-background text-foreground px-md min-h-11 border text-sm"
          value={branch}
          onChange={(event) => setBranch(event.target.value)}
        />
      </label>
      <AgentRadios prefix="project-workspace-kind" value={agentKind} onChange={setAgentKind} />
      <Button type="submit" disabled={blocked}>
        {pending ? 'Creating…' : 'New workspace'}
      </Button>
    </form>
  );
}
