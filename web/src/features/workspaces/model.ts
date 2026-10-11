/**
 * Pure rules for the workspace page: what each state allows, how a digest and a preview link are
 * read, and which failures show as notices. Kept free of React so each rule has a unit test. The
 * page shows server fields as given and never builds a host or a URL.
 */
import { z } from 'zod';
import type { components } from '@/api';
import type { LoadFailure, Notice } from '@/features/sessions/model';
import { messageTime } from '@/lib/time';

export type WorkspaceLifecycle = components['schemas']['WorkspaceLifecycle'];
export type ObservedState = WorkspaceLifecycle['observed_state'];
export type WorkspaceAction = 'suspend' | 'resume' | 'refresh' | 'delete';

export const UNCONFIRMED =
  'kagent has not confirmed this lifecycle state. Refresh status before retrying.';

/** Shown when a write (a live event or an action's result) cannot be read. The last good copy stays up. */
export const UNREADABLE =
  'Mainloop sent a workspace this page cannot read. The last good state is still shown.';

const PublicationReason = z.enum([
  'default_branch',
  'protected_branch',
  'missing_metadata',
  'no_grant',
  'disabled'
]);

// The fields the page reads. Other fields pass through untouched. A response that fails this
// check is not shown as a workspace; a bad manifest port is dropped on its own (see declaredPorts).
const WorkspaceShape = z.looseObject({
  workspace_id: z.string(),
  session_id: z.string(),
  publication_mode: z.enum(['read_only', 'branch']),
  publication_reason: PublicationReason.nullable(),
  observed_state: z.enum(['running', 'suspending', 'suspended', 'resuming', 'failed', 'unknown']),
  detail: z.string().nullable(),
  last_activity_at: z.string().nullable(),
  updated_at: z.string(),
  manifest: z.looseObject({
    repo_url: z.string(),
    ref: z.string(),
    branch: z.string(),
    agent_kind: z.string(),
    dev: z.looseObject({
      ports: z.array(z.unknown()),
      idle_timeout_minutes: z.number()
    }),
    development_environment: z.looseObject({ image: z.string(), platform: z.string() }).nullable()
  })
});

/** A workspace the page can read. A malformed one is shown as an error, not as a half-filled page. */
export function isWorkspace(value: unknown): value is WorkspaceLifecycle {
  return WorkspaceShape.safeParse(value).success;
}

const DIGEST = /^[a-z0-9]+:[0-9a-f]{8,}$/;

/**
 * The short digest of a pinned image (`repo@sha256:…`), for display. Null when the image has no
 * digest or the digest is malformed, so the page shows "unknown" instead of a broken value.
 */
export function digestPrefix(image: string): string | null {
  const at = image.indexOf('@');
  if (at === -1) return null;
  const digest = image.slice(at + 1);
  return DIGEST.test(digest) ? digest.slice(0, 19) : null;
}

export function isHttpUrl(value: string): boolean {
  try {
    const { protocol } = new URL(value);
    return protocol === 'http:' || protocol === 'https:';
  } catch {
    return false;
  }
}

const PortShape = z.looseObject({ name: z.string(), number: z.number().int() });
export type DeclaredPort = z.infer<typeof PortShape>;

/** Manifest ports that can be read. A bad one is counted and left out; the rest still show. */
export function declaredPorts(list: unknown[]): { ports: DeclaredPort[]; dropped: number } {
  const ports: DeclaredPort[] = [];
  let dropped = 0;
  for (const item of list) {
    const parsed = PortShape.safeParse(item);
    if (parsed.success) ports.push(parsed.data);
    else dropped += 1;
  }
  return { ports, dropped };
}

const PreviewShape = z.looseObject({
  port: z.number().int(),
  name: z.string(),
  url: z.string().refine(isHttpUrl)
});

export type PreviewLink = z.infer<typeof PreviewShape>;

/**
 * The preview rows the page can show. A row with a bad port, name or URL is dropped and counted,
 * and the rest still render. The URL is the server's own, used as given.
 */
export function previewLinks(list: unknown[]): { links: PreviewLink[]; dropped: number } {
  const links: PreviewLink[] = [];
  let dropped = 0;
  for (const item of list) {
    const parsed = PreviewShape.safeParse(item);
    if (parsed.success) links.push(parsed.data);
    else dropped += 1;
  }
  return { links, dropped };
}

/** Suspend is offered only from running, and resume only from suspended. */
export const canSuspend = (state: ObservedState) => state === 'running';
export const canResume = (state: ObservedState) => state === 'suspended';

/** Why the page is waiting on the runtime, when the owner should know. Null when the state is settled. */
export function stateHint(state: ObservedState): string | null {
  if (state === 'suspending' || state === 'resuming') {
    return 'Changing state. Refresh status to see where it settles.';
  }
  if (state === 'unknown') return UNCONFIRMED;
  return null;
}

/** A result whose state kagent has not confirmed must not read as success. */
export function lifecycleNotice(result: WorkspaceLifecycle): Notice | null {
  return result.observed_state === 'unknown' ? { tone: 'warn', text: UNCONFIRMED } : null;
}

/** The heading and body for a page that could not load its workspace. */
export function loadMessage(kind: LoadFailure): { title: string; message: string } {
  if (kind === 'not-found') {
    return { title: 'Workspace not found', message: 'It may have been deleted.' };
  }
  if (kind === 'server') {
    return {
      title: 'Workspace could not be loaded',
      message: 'Mainloop returned a server error.'
    };
  }
  if (kind === 'unreachable') {
    return {
      title: 'Workspace could not be loaded',
      message: "Can't reach the Mainloop backend."
    };
  }
  return { title: 'Workspace could not be loaded', message: 'Try again in a moment.' };
}

/** Last activity as a short time, or a plain word when the server has none or sent a bad value. */
export function lastActivity(value: string | null): string {
  if (!value) return 'Not recorded';
  return messageTime(value) || 'Unknown';
}
