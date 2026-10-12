import { agentLabel } from '@/lib/agentLabel';
import { useConnection } from '@/shell/ConnectionBanner';
import { liveWord, nativeProviderFor } from '@/features/sessions/model';
import { useNativeSession, useProviders } from '@/features/sessions/queries';

/**
 * The one-line identity of a session, for a health row. The full identity strip needs a whole
 * session; a health row has only the id. The native read polls only while a turn is in flight.
 */
export function HealthNativeStrip({ sessionId }: { sessionId: string }) {
  const offline = useConnection().status === 'offline';
  const native = useNativeSession(sessionId);
  const providers = useProviders();

  if (native.isError && native.data === undefined) {
    return <p className="text-muted-foreground text-xs">Session identity unavailable.</p>;
  }
  const info = native.data;
  if (!info) return null;

  const provider = agentLabel(nativeProviderFor(info.kind, providers.data ?? []));
  const model = (info.model ?? 'model pending').replace(/^claude-/, '');
  return (
    <p className="text-muted-foreground font-mono text-xs break-words">
      {provider} · {model} · {liveWord({ native: info, offline })}
    </p>
  );
}
