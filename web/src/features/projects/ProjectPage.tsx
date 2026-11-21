import { Link } from '@tanstack/react-router';
import { ChevronDown, ChevronRight } from 'lucide-react';
import type { ReactNode } from 'react';
import { useEffect, useState } from 'react';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { PAGE } from '@/components/shared/measure';
import { BACK_LINK, BackIcon, PageHeader } from '@/components/shared/page-header';
import { SkeletonHeader, SkeletonRows } from '@/components/shared/skeleton';
import { StatusBadge } from '@/components/shared/status-badge';
import { cn } from '@/components/ui/cn';
import { EnvironmentSection } from '@/features/environments/EnvironmentSection';
import { ProjectDotfilesSection } from '@/features/dotfiles/DotfilesSection';
import { SessionRow } from '@/features/sessions/SessionRow';
import { relativeTime, workspaceFor } from '@/features/sessions/model';
import {
  WorkspaceLifecycleBadge,
  WorkspacePublicationBadge
} from '@/features/sessions/WorkspaceBadges';
import { agentLabel } from '@/lib/agentLabel';
import { avatarUrl, httpsUrl } from '@/lib/url';
import { InstructionsSection } from './InstructionsSection';
import { MemorySection } from './MemorySection';
import { MergePolicySection } from './MergePolicySection';
import { VerificationSection } from './VerificationSection';
import { WorkspaceServiceSection } from './WorkspaceServiceSection';
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
  workspaceRowSchema,
  type CommitRow,
  type PullRequestRow
} from './model';
import { useProjectDetail, useProviders, useWorkspaces } from './queries';

export interface ProjectPageProps {
  id: string;
  /**
   * The project's task tree (F6). Until F6 is wired in, the section shows its empty note. The
   * supervisor passes the tree here once both slices have merged.
   */
  tasks?: ReactNode;
}

/**
 * One project: its tasks, memory, open pull requests, sessions, branch workspaces, recent commits
 * and settings. It starts nothing: new work starts from the main chat. The route keys it by id, so
 * a different project starts on a fresh page.
 */
export function ProjectPage({ id, tasks }: ProjectPageProps) {
  const detail = useProjectDetail(id);
  const workspaces = useWorkspaces();
  const providers = useProviders();
  // Settings stays folded until the owner opens it, and keeps its state while the page is open.
  const [settingsOpen, setSettingsOpen] = useState(false);
  // The header's jump links. Instructions are inside Settings, so that link opens Settings first;
  // the scroll waits for the next render, when the section is on the page.
  const [jump, setJump] = useState<{ to: string; at: number } | null>(null);
  useEffect(() => {
    if (jump) document.getElementById(jump.to)?.scrollIntoView?.({ block: 'start' });
  }, [jump]);
  const jumpTo = (to: string, openSettings = false) => {
    if (openSettings) setSettingsOpen(true);
    setJump((prev) => ({ to, at: (prev?.at ?? 0) + 1 }));
  };

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
      // The page's own column, so the header and the task rows land where the placeholders were.
      <div className={cn(PAGE, 'gap-lg')}>
        <p role="status" className="sr-only">
          Loading project...
        </p>
        <SkeletonHeader testId="project-loading" />
        <SkeletonRows rows={5} />
      </div>
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

  const avatar = avatarUrl(project.avatar_url);
  const github = httpsUrl(project.html_url);
  const pulls = keepValid(data.open_prs, pullRequestRowSchema);
  const commits = keepValid(data.recent_commits, commitRowSchema);
  const sessions = keepValid(data.sessions, sessionRowSchema);
  const providerRows = keepValid(providers.data, providerRowSchema);
  const workspaceRows = keepValid(workspaces.data, workspaceRowSchema);
  const branchWorkspaces = workspaceRows.filter(
    (workspace) => workspace.manifest.repo_url === project.html_url
  );

  return (
    <article aria-labelledby="project-heading" className={cn(PAGE, 'gap-lg')}>
      <title>{`${project.full_name} · Mainloop`}</title>
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

      {/* The shared header, so the way back and the title sit where they do on every other page. */}
      <div className="gap-xs flex min-w-0 flex-col">
        <PageHeader
          headingId="project-heading"
          title={project.full_name}
          description={project.description || undefined}
          back={
            <>
              <Link to="/" aria-label="Back to the main chat" className={BACK_LINK}>
                <BackIcon />
              </Link>
              {avatar ? <img src={avatar} alt="" className="size-8 shrink-0" /> : null}
            </>
          }
        />
        <div className="gap-xs flex min-w-0 flex-col">
          <div className="gap-sm text-muted-foreground flex flex-wrap items-center text-xs">
            {github ? (
              <a
                href={github}
                target="_blank"
                rel="noopener noreferrer"
                className="text-foreground min-h-control inline-flex items-center underline"
              >
                View on GitHub →
              </a>
            ) : null}
            <span>{openPrText(project.open_pr_count)}</span>
            <Link
              to="/health"
              search={{ project_id: id }}
              className="text-foreground min-h-control inline-flex items-center underline"
            >
              Orchestration health
            </Link>
            <Link
              to="/schedules"
              className="text-foreground min-h-control inline-flex items-center underline"
            >
              Scheduled runs
            </Link>
          </div>
          {/* The task list can be long, so what sits below it is one press away from the top. */}
          <nav
            aria-label="On this page"
            className="gap-sm text-muted-foreground flex flex-wrap items-center text-xs"
          >
            <span>On this page:</span>
            <button
              type="button"
              onClick={() => jumpTo('project-memory')}
              className="text-foreground min-h-control inline-flex items-center underline"
            >
              Memory
            </button>
            <button
              type="button"
              onClick={() => jumpTo('instructions-heading', true)}
              className="text-foreground min-h-control inline-flex items-center underline"
            >
              Instructions
            </button>
            <button
              type="button"
              onClick={() => jumpTo('project-settings-heading', true)}
              className="text-foreground min-h-control inline-flex items-center underline"
            >
              All settings
            </button>
          </nav>
        </div>
      </div>

      <section
        aria-labelledby="project-tasks-heading"
        className="gap-md flex flex-col"
        data-testid="project-tasks"
      >
        <div className="gap-sm flex flex-wrap items-center justify-between">
          <h2 id="project-tasks-heading" className="text-lg font-medium">
            Tasks
          </h2>
        </div>
        {tasks ?? <p className="text-muted-foreground text-sm">No tasks for this project yet.</p>}
      </section>

      <MemorySection projectId={id} />

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

      <section aria-labelledby="project-workspaces-heading" className="gap-md flex flex-col">
        <h2 id="project-workspaces-heading" className="text-lg font-medium">
          Branch workspaces
        </h2>
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
                  className="group gap-sm p-md border-border bg-background text-foreground hover:bg-accent hover:text-accent-foreground min-h-control flex flex-wrap items-center justify-between border"
                >
                  <span className="truncate text-sm font-medium">{workspace.manifest.branch}</span>
                  <span className="gap-sm flex flex-wrap items-center text-xs">
                    <span data-testid="workspace-agent-kind">
                      {agentLabel(agentKindOf(workspace.manifest.agent_kind))}
                    </span>
                    <WorkspaceLifecycleBadge
                      workspace={workspace}
                      className="group-hover:text-accent-foreground"
                    />
                    <WorkspacePublicationBadge
                      workspace={workspace}
                      className="group-hover:text-accent-foreground"
                    />
                    {workspace.last_activity_at ? (
                      <span className="text-muted-foreground group-hover:text-accent-foreground">
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

      <section
        aria-labelledby="project-settings-heading"
        className="gap-md flex flex-col"
        data-testid="project-settings"
      >
        <h2 id="project-settings-heading" className="text-lg font-medium">
          <button
            type="button"
            aria-expanded={settingsOpen}
            aria-controls="project-settings-body"
            onClick={() => setSettingsOpen((open) => !open)}
            className="gap-sm text-foreground hover:bg-accent min-h-control flex w-full items-center justify-between text-left"
          >
            Settings
            {settingsOpen ? (
              <ChevronDown aria-hidden="true" className="text-muted-foreground size-4" />
            ) : (
              <ChevronRight aria-hidden="true" className="text-muted-foreground size-4" />
            )}
          </button>
        </h2>
        <div id="project-settings-body" hidden={!settingsOpen}>
          <div className="gap-md flex flex-col">
            <InstructionsSection projectId={id} />
            <MergePolicySection projectId={id} />
            <VerificationSection projectId={id} />
            <EnvironmentSection projectId={id} />
            <WorkspaceServiceSection projectId={id} />
            <ProjectDotfilesSection projectId={id} />
          </div>
        </div>
      </section>
    </article>
  );
}

function PullRequestLine({ pr }: { pr: PullRequestRow }) {
  const url = httpsUrl(pr.url);
  return (
    <li className="gap-xs flex flex-col text-sm">
      <div className="gap-sm flex flex-wrap items-center">
        {url ? (
          <a
            href={url}
            target="_blank"
            rel="noopener noreferrer"
            className="min-h-control min-w-control inline-flex items-center justify-center font-medium underline"
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

function CommitLine({ commit }: { commit: CommitRow }) {
  const url = httpsUrl(commit.url);
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
            className="min-h-control min-w-control inline-flex items-center justify-center font-mono underline"
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
