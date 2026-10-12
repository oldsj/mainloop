import { ErrorRecovery } from '@/components/shared/error-recovery';
import { loadFailure } from '@/features/sessions/model';
import { loadMessage, previewLinks } from './model';
import { useWorkspacePorts } from './queries';

/**
 * Preview links for the declared HTTP ports. The URL is the server's own, used as given; the
 * client never builds a host. A row that fails the check is dropped and counted, and the rest show.
 */
export function WorkspacePreviews({ workspaceId }: { workspaceId: string }) {
  const ports = useWorkspacePorts(workspaceId);
  // Null until a response with a readable list arrives. Cached rows stay on screen through a failed refresh.
  const list = ports.data && Array.isArray(ports.data.ports) ? ports.data.ports : null;

  const reason = ports.isError
    ? loadMessage(loadFailure(ports.error)).message
    : ports.data !== undefined && list === null
      ? 'Mainloop sent preview ports this page cannot read.'
      : null;

  const { links, dropped } = previewLinks(list ?? []);

  return (
    <section aria-labelledby="previews-heading" className="gap-sm flex flex-col">
      <h2 id="previews-heading" className="text-base font-medium">
        Previews
      </h2>
      <p className="text-muted-foreground text-sm">
        Open a declared HTTP port in a new tab. A suspended workspace wakes when you open it.
      </p>

      {reason ? (
        <ErrorRecovery
          title="Preview ports could not be loaded."
          message={reason}
          onRetry={() => void ports.refetch()}
          pending={ports.isFetching}
        />
      ) : null}

      {list === null && !reason ? (
        <p role="status" className="text-muted-foreground text-sm">
          Loading previews…
        </p>
      ) : null}

      {list !== null && links.length === 0 && dropped === 0 ? (
        <p className="text-muted-foreground text-sm">No preview ports are declared.</p>
      ) : null}

      {links.length > 0 ? (
        <ul className="flex flex-col">
          {links.map((link) => (
            <li
              key={`${link.port}-${link.url}`}
              className="gap-md border-border py-sm flex flex-wrap items-center justify-between border-b"
            >
              <span className="min-w-0 text-sm break-words">
                {link.name}
                <span className="text-muted-foreground font-mono">:{link.port}</span>
              </span>
              <a
                href={link.url}
                target="_blank"
                rel="noopener noreferrer"
                aria-label={`Open preview ${link.name}, port ${link.port}`}
                className="text-foreground inline-flex min-h-11 items-center text-sm underline"
              >
                Open preview
              </a>
            </li>
          ))}
        </ul>
      ) : null}

      {dropped > 0 ? (
        <p role="status" className="text-muted-foreground text-xs">
          {dropped === 1
            ? 'One preview could not be read and is not listed.'
            : `${dropped} previews could not be read and are not listed.`}
        </p>
      ) : null}
    </section>
  );
}
