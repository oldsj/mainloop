import { deliveryFixture, mainThreadFixture, nativeFixture, providerFixture } from './fixtures';
import { MainThreadHeader } from './MainThreadHeader';
import type { MainProviderControl } from './mainProvider';
import type { MainProviderInfo, MainThreadInfo } from './types';

const topics = [
  { name: 'build', status_line: 'running', pending: 1 },
  { name: 'docs', status_line: 'done', pending: 0 }
];

/** The controls with nothing held and no notice: the owner can select, retry or restart. */
const idleControl: MainProviderControl = {
  selecting: false,
  unconfirmed: false,
  switchBlocked: false,
  notice: null,
  retryOffered: false,
  restartOffered: false,
  select: () => {},
  retry: () => {},
  restart: () => {}
};

function thread(provider: MainProviderInfo | null): MainThreadInfo {
  return mainThreadFixture({
    topics,
    provider,
    native: nativeFixture({ deliveries: [deliveryFixture({ state: 'completed' })] })
  });
}

/** Ready, working with stop, last message failed, unconfirmed, and unreachable. */
export default function MainThreadHeaderPreview() {
  const turn = mainThreadFixture({
    topics,
    native: nativeFixture({ turn_in_flight: true, deliveries: [deliveryFixture({ state: 'sending' })] })
  });
  const failed = mainThreadFixture({
    topics,
    native: nativeFixture({ deliveries: [deliveryFixture({ state: 'failed' })] })
  });
  const uncertain = mainThreadFixture({
    topics,
    native: nativeFixture({ deliveries: [deliveryFixture({ state: 'uncertain' })] })
  });
  return (
    <div className="gap-md flex flex-col">
      <MainThreadHeader info={thread(providerFixture())} offline={false} provider={idleControl} />
      <MainThreadHeader info={turn} offline={false} onStop={() => {}} provider={{ ...idleControl, switchBlocked: true }} />
      <MainThreadHeader info={failed} offline={false} provider={idleControl} />
      <MainThreadHeader info={uncertain} offline={false} provider={{ ...idleControl, switchBlocked: true }} />
      <MainThreadHeader info={thread(providerFixture())} offline provider={{ ...idleControl, switchBlocked: true }} />
    </div>
  );
}

/** The provider row for a `not_started` main thread: it starts with the owner's first message. */
export function ProviderNotStartedPreview() {
  const info = thread(providerFixture({ status: 'not_started', effective_provider: null }));
  return <MainThreadHeader info={info} offline={false} provider={idleControl} />;
}

/** The provider row while a selection is preparing: the select is held. */
export function ProviderPendingPreview() {
  const info = thread(providerFixture({ desired_provider: 'codex', effective_provider: 'claude', status: 'pending' }));
  return <MainThreadHeader info={info} offline={false} provider={{ ...idleControl, switchBlocked: true }} />;
}

/** The provider row for a ready, confirmed provider. */
export function ProviderReadyPreview() {
  return <MainThreadHeader info={thread(providerFixture())} offline={false} provider={idleControl} />;
}

/** The provider row for a failed start: Retry, and Restart because the failure is recoverable. */
export function ProviderFailedPreview() {
  const info = thread(
    providerFixture({
      desired_provider: 'codex',
      effective_provider: null,
      status: 'failed',
      error: 'The Codex runtime did not start.',
      recoverable: true
    })
  );
  return (
    <MainThreadHeader
      info={info}
      offline={false}
      provider={{
        ...idleControl,
        retryOffered: true,
        restartOffered: true,
        notice: 'Provider selection was not confirmed: request failed. State is refreshed; no automatic retry.'
      }}
    />
  );
}
