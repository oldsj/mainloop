import { Link } from '@tanstack/react-router';
import { LoaderCircle } from 'lucide-react';
import { StatusBadge } from '@/components/shared/status-badge';
import { agentLabel } from '@/lib/agentLabel';
import type { ProviderProfile, Session } from '@/lib/contract';
import { statusLabel } from '@/lib/sessionStatus';
import { nativeProviderFor, relativeTime, sessionTone, type WorkspaceLifecycle } from './model';
import { WorkspaceLifecycleBadge } from './WorkspaceBadges';

// Text on the `bg-accent` fill of the open row (`aria-current`) and of a hovered row. Muted and
// status colours give way to accent-foreground there, so every word on the fill stays at 4.5:1.
const ON_FILL =
  'group-hover:text-accent-foreground group-aria-[current=page]:text-accent-foreground';

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
      className="group gap-xs border-border bg-background text-foreground hover:bg-accent hover:text-accent-foreground p-md flex min-h-11 flex-col border"
    >
      <div className="gap-sm flex items-center justify-between">
        <div className="gap-sm flex min-w-0 items-center">
          {/* The open row's marker. It stays transparent on other rows, so the titles line up. */}
          <span
            aria-hidden="true"
            className="group-aria-[current=page]:text-accent-foreground text-transparent"
          >
            ▌
          </span>
          {running ? (
            <LoaderCircle
              className={`text-term-cyan size-3 shrink-0 animate-spin ${ON_FILL}`}
              aria-hidden="true"
            />
          ) : null}
          <h3 className="truncate text-sm font-medium group-aria-[current=page]:font-bold">
            {session.parent_session_id ? <span data-testid="child-marker">↳ </span> : null}
            {session.title}
          </h3>
          {session.topic ? (
            <span
              data-testid="session-topic"
              className={`text-muted-foreground shrink-0 text-xs ${ON_FILL}`}
            >
              #{session.topic}
            </span>
          ) : null}
        </div>
        <time
          dateTime={session.created_at}
          className={`text-muted-foreground shrink-0 text-xs ${ON_FILL}`}
        >
          {relativeTime(session.created_at)}
        </time>
      </div>

      <div className="gap-sm flex flex-wrap items-center justify-between">
        <div className="gap-sm flex min-w-0 flex-wrap items-center">
          <StatusBadge
            label={statusLabel(session.status, session.needs_owner)}
            tone={sessionTone(session)}
            className={ON_FILL}
          />
          <span data-testid="session-kind" className={`text-muted-foreground text-xs ${ON_FILL}`}>
            {kind}
          </span>
          {workspace ? <WorkspaceLifecycleBadge workspace={workspace} className={ON_FILL} /> : null}
        </div>
        {repo ? (
          <span className={`text-muted-foreground truncate text-xs ${ON_FILL}`}>{repo}</span>
        ) : null}
      </div>

      {session.error ? (
        <div
          className={`text-destructive truncate text-xs ${ON_FILL}`}
        >{`Error: ${session.error}`}</div>
      ) : null}
    </Link>
  );
}
