import { useEffect, useRef, useState } from 'react';
import { $api, ApiError } from '@/api';
import { ErrorRecovery } from '@/components/shared/error-recovery';

/**
 * Where a preview host sends a page load that has no preview cookie (docs/specs/workspaces.md,
 * "Preview sign-in"). The owner is signed in to the app, so this asks the API for a one-minute
 * sign-in for that one preview host and opens the URL it returns. The sign-in rides in that URL's
 * fragment, which the browser never sends to a server.
 */

/** Opens of one preview within LOOP_WINDOW_MS before the page stops and says so. */
export const LOOP_LIMIT = 3;
export const LOOP_WINDOW_MS = 60_000;
const LOOP_KEY = 'mainloop.preview-signin';

type Store = Pick<Storage, 'getItem' | 'setItem'>;

function sessionStore(): Store | null {
  try {
    return window.sessionStorage;
  } catch {
    return null;
  }
}

/**
 * Records one open of `target` and reports whether it is one too many: a browser that refuses the
 * preview cookie would otherwise bounce between the preview and this page forever.
 */
export function tooManyOpens(target: string, now: number, store: Store | null): boolean {
  if (!store) return false;
  let seen: Record<string, number[]> = {};
  try {
    const parsed: unknown = JSON.parse(store.getItem(LOOP_KEY) ?? '{}');
    if (parsed && typeof parsed === 'object') seen = parsed as Record<string, number[]>;
  } catch {
    seen = {};
  }
  const recent = (Array.isArray(seen[target]) ? seen[target] : []).filter(
    (t) => typeof t === 'number' && now - t < LOOP_WINDOW_MS
  );
  recent.push(now);
  seen[target] = recent;
  try {
    store.setItem(LOOP_KEY, JSON.stringify(seen));
  } catch {
    // Storage full or blocked: the guard is best effort.
  }
  return recent.length > LOOP_LIMIT;
}

/** Only an http(s) URL from the API is opened. */
export function openable(url: unknown): url is string {
  if (typeof url !== 'string') return false;
  try {
    const u = new URL(url);
    return u.protocol === 'https:' || u.protocol === 'http:';
  } catch {
    return false;
  }
}

function failureMessage(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.status === 404)
      return 'This workspace has no running preview. It may have been deleted or finished.';
    if (error.status === 422) return error.message;
    if (error.status === 401 || error.status === 403)
      return 'Sign in to Mainloop to open this preview.';
    return error.message;
  }
  return "Couldn't reach Mainloop. Try again when it's back.";
}

export function PreviewSignIn({
  workspaceId,
  port,
  path,
  open = (url: string) => window.location.replace(url),
  now = Date.now,
  store = sessionStore()
}: {
  workspaceId: string;
  port: number;
  path: string;
  open?: (url: string) => void;
  now?: () => number;
  store?: Store | null;
}) {
  const grant = $api.useMutation('post', '/workspaces/{workspace_id}/previews/{port}/grant');
  const validPort = Number.isInteger(port) && port >= 1 && port <= 65535;
  // Counted once per mount, before the first request.
  const [looping, setLooping] = useState(
    () => validPort && tooManyOpens(`${workspaceId}:${port}`, now(), store)
  );
  const [failure, setFailure] = useState<string | null>(null);
  const started = useRef(false);

  const start = () => {
    setLooping(false);
    setFailure(null);
    grant.mutate(
      { params: { path: { workspace_id: workspaceId, port } }, body: { path } },
      {
        onSuccess: (result) => {
          if (openable(result?.url)) open(result.url);
          else setFailure('Mainloop sent a preview address this page cannot open.');
        },
        onError: (error) => setFailure(failureMessage(error))
      }
    );
  };

  useEffect(() => {
    if (started.current || !validPort || looping) return;
    started.current = true;
    start();
    // Runs once per mount; the retry button starts again by hand.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const problem = !validPort
    ? 'This preview address names no valid port.'
    : looping
      ? 'The preview keeps asking to sign in. Check that this browser allows cookies for the preview, then try again.'
      : failure;

  return (
    <main className="gap-md p-lg mx-auto flex max-w-prose flex-col">
      <h1 className="text-base font-medium">Opening preview</h1>
      <p className="text-muted-foreground text-sm">
        Port <span className="font-mono">{port}</span> of workspace{' '}
        <span className="font-mono break-all">{workspaceId}</span>
      </p>
      {problem ? (
        <ErrorRecovery
          title="The preview could not be opened."
          message={problem}
          onRetry={validPort ? start : undefined}
          pending={grant.isPending}
        />
      ) : (
        <p role="status" className="text-muted-foreground text-sm">
          Signing in to the preview…
        </p>
      )}
    </main>
  );
}
