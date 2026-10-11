import { Link, useNavigate } from '@tanstack/react-router';
import { useState } from 'react';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { Button } from '@/components/ui/button';
import { safeExternalUrl } from '@/lib/taskState';
import { useConnection } from '@/shell/ConnectionBanner';
import { AgentRadios } from './AgentRadios';
import { CreateCheck } from './CreateCheck';
import {
  failureText,
  keepValid,
  projectRowSchema,
  workspaceRequest,
  type AgentKind,
  type Project
} from './model';
import { useCreateWorkspace, useProjects } from './queries';

/** The Projects panel: the owner's repositories, and a way to start a workspace from any repository. */
export function ProjectsPanel() {
  const navigate = useNavigate();
  const projects = useProjects();
  const create = useCreateWorkspace();
  const uncertain = create.uncertain;
  const [open, setOpen] = useState(false);
  const rows = keepValid<Project>(projects.data, projectRowSchema);

  const submit = (target: { repo: string; branch: string; agentKind: AgentKind }) => {
    if (create.pending || uncertain) return;
    create.mutate(
      { body: workspaceRequest(target) },
      {
        onSuccess: (workspace) =>
          void navigate({ to: '/workspaces/$id', params: { id: workspace.workspace_id } })
      }
    );
  };

  return (
    <section
      aria-labelledby="projects-heading"
      className="gap-md p-md flex flex-col"
      data-testid="projects-panel"
    >
      <div className="gap-sm flex flex-wrap items-center justify-between">
        <h2 id="projects-heading" className="text-sm font-medium">
          Projects
        </h2>
        <Button
          type="button"
          variant="outline"
          aria-expanded={open}
          aria-controls="new-workspace-form"
          onClick={() => setOpen((value) => !value)}
        >
          {open ? 'Cancel' : 'New workspace'}
        </Button>
      </div>

      {open ? (
        <NewWorkspaceForm pending={create.pending} locked={uncertain !== null} onSubmit={submit} />
      ) : null}

      {uncertain ? (
        <CreateCheck
          repo={uncertain.repo}
          branch={uncertain.branch}
          reading={create.pending}
          onAcknowledge={create.acknowledge}
        />
      ) : create.error ? (
        <div role="alert" className="text-destructive text-sm">
          {failureText(create.error)}
        </div>
      ) : null}

      {projects.isPending ? (
        <p role="status" className="text-muted-foreground text-sm">
          Loading projects…
        </p>
      ) : null}

      {projects.isError && !projects.data ? (
        <ErrorRecovery
          title="Couldn't load projects"
          message={failureText(projects.error)}
          onRetry={() => void projects.refetch()}
          retryLabel="Retry loading projects"
          pending={projects.isFetching}
        />
      ) : null}

      {projects.isError && projects.data ? (
        <div data-testid="projects-refresh-error">
          <ErrorRecovery
            title="Couldn't refresh projects"
            message={`${failureText(projects.error)} Showing the last list.`}
            onRetry={() => void projects.refetch()}
            retryLabel="Retry loading projects"
            pending={projects.isFetching}
          />
        </div>
      ) : null}

      {projects.isSuccess && rows.length === 0 ? (
        <p className="text-muted-foreground text-sm">No projects yet</p>
      ) : null}

      {rows.length > 0 ? (
        <ul data-testid="projects-list" className="gap-sm grid md:grid-cols-2">
          {rows.map((project) => (
            <ProjectRow key={project.id} project={project} />
          ))}
        </ul>
      ) : null}
    </section>
  );
}

function ProjectRow({ project }: { project: Project }) {
  const avatar = safeExternalUrl(project.avatar_url);
  return (
    <li>
      <Link
        to="/projects/$id"
        params={{ id: project.id }}
        className="gap-sm p-md border-border bg-background text-foreground hover:bg-accent flex min-h-11 items-center border"
      >
        {avatar ? (
          <img src={avatar} alt="" className="size-8 shrink-0" />
        ) : (
          <span aria-hidden="true" className="bg-muted size-8 shrink-0" />
        )}
        <span className="gap-xs flex min-w-0 flex-col">
          <span className="truncate text-sm font-medium">{project.full_name}</span>
          {project.description ? (
            <span className="text-muted-foreground truncate text-xs">{project.description}</span>
          ) : null}
        </span>
      </Link>
    </li>
  );
}

/**
 * Starts a workspace from a repository the owner types. The parent owns the request, so a failed
 * create still shows in the panel after this form closes.
 */
function NewWorkspaceForm({
  pending,
  locked,
  onSubmit
}: {
  pending: boolean;
  /** An earlier create may have started a workspace; no new one until the owner has checked. */
  locked: boolean;
  onSubmit: (target: { repo: string; branch: string; agentKind: AgentKind }) => void;
}) {
  const [repo, setRepo] = useState('');
  const [branch, setBranch] = useState('');
  const [agentKind, setAgentKind] = useState<AgentKind>('claude');
  const [problem, setProblem] = useState<string | null>(null);
  const { status } = useConnection();
  const offline = status === 'offline';
  const blocked = offline || pending || locked;

  return (
    <form
      id="new-workspace-form"
      data-testid="new-workspace-form"
      className="gap-md p-md border-border flex flex-col border"
      onSubmit={(event) => {
        event.preventDefault();
        if (!repo.trim()) {
          setProblem('Enter a repository');
          return;
        }
        setProblem(null);
        if (blocked) return;
        onSubmit({ repo: repo.trim(), branch: branch.trim(), agentKind });
      }}
    >
      <label className="gap-xs flex flex-col text-sm">
        Repository
        <input
          aria-label="Repository"
          className="border-border bg-background text-foreground px-md min-h-11 border text-sm"
          placeholder="owner/name or https://github.com/owner/name"
          value={repo}
          onChange={(event) => setRepo(event.target.value)}
        />
      </label>

      <label className="gap-xs flex flex-col text-sm">
        Branch (optional)
        <input
          aria-label="Branch (optional)"
          className="border-border bg-background text-foreground px-md min-h-11 border text-sm"
          value={branch}
          onChange={(event) => setBranch(event.target.value)}
        />
      </label>

      <AgentRadios prefix="new-workspace-kind" value={agentKind} onChange={setAgentKind} />

      {problem ? (
        <p role="alert" className="text-destructive text-sm">
          {problem}
        </p>
      ) : null}

      <Button type="submit" disabled={blocked}>
        {pending ? 'Creating…' : 'Create workspace'}
      </Button>
    </form>
  );
}
