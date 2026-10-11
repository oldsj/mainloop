import { http, HttpResponse, type HttpResponseResolver } from 'msw';
import { API_URL } from '@/api/config';
import type { components } from '@/api/schema.gen';
import type { ProviderProfile } from '@/lib/contract';
import { CLAUDE_PROFILE, CODEX_PROFILE } from '@/test/fixtures/tasks';
import type { WorkspaceLifecycle } from '../model';
import { attentionSnapshot, native, type NativeFixture, type SessionFixture } from './fixtures';

type Schemas = components['schemas'];
type Action = 'cancel' | 'stop-turn' | 'archive';

const url = (path: string) => `${API_URL}${path}`;

export const attentionRead = (resolve: HttpResponseResolver) =>
  http.get(url('/attention'), resolve);

export const sessionRead = (resolve: HttpResponseResolver) =>
  http.get(url('/sessions/:session_id'), resolve);

export const nativeRead = (info: NativeFixture) =>
  http.get(url('/sessions/:session_id/native'), () => HttpResponse.json(info));

/** A native read with a resolver of its own: a body the client does not expect, or one per session. */
export const nativeReadWith = (resolve: HttpResponseResolver) =>
  http.get(url('/sessions/:session_id/native'), resolve);

export const sessionAction = (action: Action, resolve: HttpResponseResolver) =>
  http.post(url(`/sessions/:session_id/${action}`), resolve);

export const createSession = (resolve: HttpResponseResolver) =>
  http.post(url('/sessions'), resolve);

export const providersRead = (list: ProviderProfile[]) =>
  http.get(url('/providers'), () => HttpResponse.json(list));

export const tasksRead = (list: Schemas['TaskView'][]) =>
  http.get(url('/tasks'), () => HttpResponse.json(list));

export const workspacesRead = (list: WorkspaceLifecycle[]) =>
  http.get(url('/workspaces'), () => HttpResponse.json(list));

/** A non-2xx response with the backend's `{ detail }` body, as the client reads it. */
export function problem(status: number, detail: string): HttpResponseResolver {
  return () => HttpResponse.json({ detail }, { status });
}

/** The reads the list and the session page make besides attention: providers and tasks. */
export function otherListReads() {
  return [providersRead([CLAUDE_PROFILE, CODEX_PROFILE]), tasksRead([])];
}

/**
 * Handlers for the sessions list: attention lists these sessions. Put a test's overrides before
 * these in `server.use`, since the first matching handler wins.
 */
export function serveList(sessions: SessionFixture[]) {
  return [attentionRead(() => HttpResponse.json(attentionSnapshot(sessions))), ...otherListReads()];
}

/**
 * Handlers for one session's page with its native reads left to the chat backend. A test can then
 * change the backend's state (a turn in flight, a lost reply) after the page has loaded.
 */
export function serveSessionChat(item: SessionFixture) {
  return [...serveList([item]), sessionRead(() => HttpResponse.json(item))];
}

/**
 * Handlers for one session's page: the session is both in attention and a direct read, and its
 * native state is `info`. Like `serveList`, a test's own overrides go first.
 */
export function serveSession(
  item: SessionFixture,
  info: NativeFixture = native({ session_id: item.id })
) {
  return [...serveList([item]), sessionRead(() => HttpResponse.json(item)), nativeRead(info)];
}
