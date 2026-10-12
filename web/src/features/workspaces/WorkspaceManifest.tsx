import type { ReactNode } from 'react';
import { httpUrl } from '@/lib/url';
import { declaredPorts, digestPrefix, lastActivity, type WorkspaceLifecycle } from './model';

/**
 * The facts the workspace was declared with. Every value is shown as the server sent it; the only
 * derived values are the short image digest (`digestPrefix`) and the port list, which skips rows it
 * cannot read and says how many it skipped.
 */
export function WorkspaceManifest({ workspace }: { workspace: WorkspaceLifecycle }) {
  const { manifest } = workspace;
  const { ports, dropped } = declaredPorts(manifest.dev.ports);
  const environment = manifest.development_environment;
  const repoHref = httpUrl(manifest.repo_url);

  return (
    <section aria-labelledby="manifest-heading" className="gap-sm flex flex-col">
      <h2 id="manifest-heading" className="text-base font-medium">
        Manifest
      </h2>
      <dl className="gap-sm flex flex-col text-sm">
        <Fact label="Repository">
          {manifest.repo_url === '' ? (
            'Not declared'
          ) : repoHref ? (
            <a
              href={repoHref}
              target="_blank"
              rel="noopener noreferrer"
              className="inline-flex min-h-11 items-center break-all underline"
            >
              {manifest.repo_url}
            </a>
          ) : (
            manifest.repo_url
          )}
        </Fact>
        <Fact label="Branch">{manifest.branch}</Fact>
        <Fact label="Development environment">
          {environment ? (
            <span title={environment.image}>
              {digestPrefix(environment.image) ?? 'unknown'} · {environment.platform}
            </span>
          ) : (
            'Not declared'
          )}
        </Fact>
        <Fact label="Base ref">{manifest.ref || 'Default'}</Fact>
        <Fact label="Agent">{manifest.agent_kind}</Fact>
        <Fact label="Idle timeout">{`${manifest.dev.idle_timeout_minutes} minutes`}</Fact>
        <Fact label="Last activity">{lastActivity(workspace.last_activity_at)}</Fact>
        <Fact label="Preview ports">
          {ports.length === 0
            ? 'None declared'
            : ports.map((port) => `${port.name}: ${port.number}`).join(', ')}
        </Fact>
      </dl>
      {dropped > 0 ? (
        <p role="status" className="text-muted-foreground text-xs">
          {dropped === 1
            ? 'One declared port could not be read and is not listed.'
            : `${dropped} declared ports could not be read and are not listed.`}
        </p>
      ) : null}
    </section>
  );
}

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="gap-xs sm:gap-md flex flex-col sm:flex-row sm:items-baseline">
      <dt className="text-muted-foreground shrink-0 sm:w-44">{label}</dt>
      <dd className="text-foreground min-w-0 break-words">{children}</dd>
    </div>
  );
}
