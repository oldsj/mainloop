import { useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { $api, keys } from '@/api';
import { clearDraft } from './drafts';
import { describeEffort, resolveEffortCommand } from './effortCommand';
import { describeModel, resolveModelCommand } from './modelCommand';
import { profileForKind, useProviderProfiles } from './providerProfiles';
import { parseSlashCommand } from './slashCommand';
import type { NativeSessionInfo } from './types';

/**
 * `/model` and `/effort`: local commands that never become chat messages. Shared by the main
 * thread and a session's own chat, since both are sessions with their own sticky override (see
 * docs/specs/chat.md). The write applies from the session's next turn; nothing here waits for it.
 */
export function useModelEffortCommands(
  sessionId: string | null | undefined,
  native: NativeSessionInfo | null | undefined,
  draftKey: string
) {
  const queryClient = useQueryClient();
  const profiles = useProviderProfiles();
  const profile = profileForKind(profiles.data, native?.kind);
  const setModel = $api.useMutation('post', '/sessions/{session_id}/model');
  const setEffort = $api.useMutation('post', '/sessions/{session_id}/effort');
  const [commandNotice, setCommandNotice] = useState<string | null>(null);

  function refresh() {
    if (!sessionId) return;
    void queryClient.invalidateQueries({ queryKey: keys.sessionNative(sessionId) });
    void queryClient.invalidateQueries({ queryKey: keys.mainThread() });
  }

  /** Intercepts a composer send for `/model` or `/effort`. Returning null defers to an ordinary send. */
  function intercept(text: string): Promise<boolean> | null {
    const command = parseSlashCommand(text);
    if (!command || (command.name !== 'model' && command.name !== 'effort')) {
      if (commandNotice) setCommandNotice(null);
      return null;
    }
    if (!sessionId) {
      setCommandNotice(
        command.name === 'model' ? describeModel(native, profile) : describeEffort(native, profile)
      );
      clearDraft(draftKey);
      return Promise.resolve(true);
    }
    if (command.name === 'model') {
      const outcome = resolveModelCommand(command.args, native, profile);
      if ('notice' in outcome) {
        setCommandNotice(outcome.notice);
      } else {
        setCommandNotice(
          outcome.model
            ? `Model set to ${outcome.model}. Takes effect next turn.`
            : 'Model reset to default. Takes effect next turn.'
        );
        setModel.mutate(
          { params: { path: { session_id: sessionId } }, body: { model: outcome.model } },
          {
            onSuccess: refresh,
            onError: (error) => setCommandNotice(`Could not set the model: ${errorDetail(error)}`)
          }
        );
      }
    } else {
      const outcome = resolveEffortCommand(command.args, native, profile);
      if ('notice' in outcome) {
        setCommandNotice(outcome.notice);
      } else {
        setCommandNotice(
          outcome.effort
            ? `Effort set to ${outcome.effort}. Takes effect next turn.`
            : 'Effort reset to default. Takes effect next turn.'
        );
        setEffort.mutate(
          { params: { path: { session_id: sessionId } }, body: { effort: outcome.effort } },
          {
            onSuccess: refresh,
            onError: (error) => setCommandNotice(`Could not set the effort: ${errorDetail(error)}`)
          }
        );
      }
    }
    clearDraft(draftKey);
    return Promise.resolve(true);
  }

  return { commandNotice, intercept };
}

/** The server's reason in an error body (`{"detail": "..."}`), when it has one. */
function errorDetail(error: unknown): string {
  if (error && typeof error === 'object' && 'detail' in error) {
    const detail = (error as { detail?: unknown }).detail;
    if (typeof detail === 'string' && detail) return detail;
  }
  return 'request failed';
}
