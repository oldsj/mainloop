import type { MainThreadInfo, MainProviderInfo, MainProvider } from './api';

export function providerSwitchBlocked(info: MainThreadInfo | null, busy: boolean): boolean {
  return (
    busy ||
    !info?.provider ||
    info.provider.status === 'pending' ||
    !!info.native?.turn_in_flight ||
    !!info.native?.deliveries.some((d) =>
      ['recorded', 'sending', 'delivered', 'uncertain'].includes(d.state)
    )
  );
}

export function providerReady(provider?: MainProviderInfo | null): boolean {
  return provider?.status === 'ready' && provider.effective_provider === provider.desired_provider;
}

/** One explicit write, followed by a read even when the write outcome is unknown. */
export async function selectMainProvider(
  provider: MainProvider,
  restartFailed: boolean,
  put: (provider: MainProvider, restartFailed: boolean) => Promise<MainProviderInfo>,
  refresh: () => Promise<unknown>,
  report: (message: string) => void
): Promise<boolean> {
  try {
    await put(provider, restartFailed);
  } catch (error) {
    const conflict = error instanceof Error && 'status' in error && error.status === 409;
    report(
      `${conflict ? 'Provider change blocked (409)' : 'Provider selection was not confirmed'}: ${error instanceof Error ? error.message : 'request failed'}. State will be refreshed; no automatic retry.`
    );
  }
  try {
    await refresh();
    return true;
  } catch {
    report(
      'Could not confirm provider state. Waiting for a successful refresh before allowing another selection or main message.'
    );
    return false;
  }
}

/** Invalidates reads started before a selection, and rejects out-of-order polling results. */
export function createMainThreadReads() {
  let version = 0;
  return {
    invalidate: () => {
      version++;
    },
    read: async (fetch: () => Promise<MainThreadInfo>, apply: (info: MainThreadInfo) => void) => {
      const current = ++version;
      const info = await fetch();
      if (current === version) apply(info);
      return info;
    }
  };
}
