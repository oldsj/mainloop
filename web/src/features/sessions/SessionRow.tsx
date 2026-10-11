import { Link } from '@tanstack/react-router';
import { LoaderCircle } from 'lucide-react';
import { StatusBadge } from '@/components/shared/status-badge';
import { agentLabel } from '@/lib/agentLabel';
import type { ProviderProfile, Session } from '@/lib/contract';
import { statusLabel } from '@/lib/sessionStatus';
import { nativeProviderFor, relativeTime, sessionTone, type WorkspaceLifecycle } from './model';
import { WorkspaceLifecycleBadge } from './WorkspaceBadges';

export interface SessionRowProps {
  session: Session;
  workspace?: WorkspaceLifecycle;
  providers: ProviderProfile[];
}

/**
 * One session in the list. The whole row is the link to its page; the router marks it
 * `aria-current` while open. The `tab` search param is cleared so that a tap in the compact shell
 * opens the page rather than leaving the sessions tab showing.
 */
export function SessionRow({ session, workspace, providers }: SessionRowProps) {
  const running = session.status === 'active';
  const kind = agentLabel(
    nativeProviderFor(workspace?.manifest.agent_kind ?? session.agent_kind, providers)
  );
  const repo = session.repo_url?.replace('https://github.com/', '');

  return (
    <Link
      to="/sessions/$id"
      params={{ id: session.id }}
      search={(prev) => ({ ...prev, tab: undefined })}
      activeProps={{ className: 'bg-accent text-accent-foreground' }}
      className="gap-xs border-border bg-background text-foreground hover:bg-accent p-md flex min-h-11 flex-col border"
    >
      <div className="gap-sm flex items-center justify-between">
        <div className="gap-sm flex min-w-0 items-center">
          {running ? (
            <LoaderCircle
              className="text-term-cyan size-3 shrink-0 animate-spin"
              aria-hidden="true"
            />
          ) : null}
          <h3 className="truncate text-sm font-medium">
            {session.parent_session_id ? <span data-testid="child-marker">↳ </span> : null}
            {session.title}
          </h3>
          {session.topic ? (
            <span data-testid="session-topic" className="text-muted-foreground shrink-0 text-xs">
              #{session.topic}
            </span>
          ) : null}
        </div>
        <time dateTime={session.created_at} className="text-muted-foreground shrink-0 text-xs">
          {relativeTime(session.created_at)}
        </time>
      </div>

      <div className="gap-sm flex flex-wrap items-center justify-between">
        <div className="gap-sm flex min-w-0 flex-wrap items-center">
          <StatusBadge
            label={statusLabel(session.status, session.needs_owner)}
            tone={sessionTone(session)}
          />
          <span data-testid="session-kind" className="text-muted-foreground text-xs">
            {kind}
          </span>
          {workspace ? <WorkspaceLifecycleBadge workspace={workspace} /> : null}
        </div>
        {repo ? <span className="text-muted-foreground truncate text-xs">{repo}</span> : null}
      </div>

      {session.error ? (
        <div className="text-destructive truncate text-xs">{`Error: ${session.error}`}</div>
      ) : null}
    </Link>
  );
}
