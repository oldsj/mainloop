import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { deliveryFixture, mainThreadFixture, nativeFixture, providerFixture } from './fixtures';
import type { MainProviderControl } from './mainProvider';
import { MainThreadHeader } from './MainThreadHeader';
import type { MainProviderInfo, MainThreadInfo } from './types';

const view = (
  info: MainThreadInfo,
  props: Partial<React.ComponentProps<typeof MainThreadHeader>> = {}
) => render(<MainThreadHeader info={info} offline={false} {...props} />);

describe('MainThreadHeader', () => {
  it('shows ready, the model without its vendor prefix, and no pending count', () => {
    view(mainThreadFixture());
    expect(screen.getByTestId('mt-state')).toHaveTextContent('ready');
    expect(screen.getByTestId('mt-model')).toHaveTextContent('sonnet-5-5');
    expect(screen.queryByTestId('mt-pending')).toBeNull();
  });

  it('falls back to the agent kind when there is no model', () => {
    view(mainThreadFixture({ native: nativeFixture({ model: null, kind: 'codex' }) }));
    expect(screen.getByTestId('mt-model')).toHaveTextContent('codex');
  });

  it('sums pending over topics', () => {
    view(
      mainThreadFixture({
        topics: [
          { name: 'a', status_line: 'running', pending: 2 },
          { name: 'b', status_line: '', pending: 1 }
        ]
      })
    );
    expect(screen.getByTestId('mt-pending')).toHaveTextContent('3 pending');
  });

  it.each([
    ['sending', 'working'],
    ['failed', 'last message failed'],
    ['uncertain', 'last message unconfirmed']
  ])('shows a %s delivery as "%s"', (state, words) => {
    view(
      mainThreadFixture({ native: nativeFixture({ deliveries: [deliveryFixture({ state })] }) })
    );
    expect(screen.getByTestId('mt-state')).toHaveTextContent(words);
  });

  it('shows a suspended session as idle and an unreachable backend as unreachable', () => {
    const { rerender } = view(
      mainThreadFixture({ native: nativeFixture({ session_state: 'suspended' }) })
    );
    expect(screen.getByTestId('mt-state')).toHaveTextContent('idle');
    rerender(<MainThreadHeader info={mainThreadFixture()} offline />);
    expect(screen.getByTestId('mt-state')).toHaveTextContent('unreachable');
  });

  it('offers stop only while a turn is in flight', async () => {
    const onStop = vi.fn();
    const { rerender } = view(mainThreadFixture(), { onStop });
    expect(screen.queryByTestId('mt-stop-turn')).toBeNull();
    rerender(
      <MainThreadHeader
        info={mainThreadFixture({ native: nativeFixture({ turn_in_flight: true }) })}
        offline={false}
        onStop={onStop}
      />
    );
    await userEvent.click(screen.getByTestId('mt-stop-turn'));
    expect(onStop).toHaveBeenCalledOnce();
  });

  it('shows stopping… and disables stop while the request runs', () => {
    view(mainThreadFixture({ native: nativeFixture({ turn_in_flight: true }) }), {
      onStop: vi.fn(),
      stopping: true
    });
    expect(screen.getByTestId('mt-stop-turn')).toHaveTextContent('stopping…');
    expect(screen.getByTestId('mt-stop-turn')).toBeDisabled();
  });

  it('lists the topics under details', async () => {
    view(
      mainThreadFixture({
        topics: [{ name: 'build', status_line: 'running', pending: 1 }]
      }),
      { renderIdentity: (id) => <div data-testid="identity-strip">{id}</div> }
    );
    expect(screen.queryByTestId('topic-index')).toBeNull();
    await userEvent.click(screen.getByTestId('mt-details-toggle'));
    expect(screen.getByTestId('topic-line')).toHaveTextContent('build (running) [1 pending]');
    expect(screen.getByTestId('identity-strip')).toHaveTextContent('sess-main');
  });

  it('says there are no topics yet', async () => {
    view(mainThreadFixture());
    await userEvent.click(screen.getByTestId('mt-details-toggle'));
    expect(screen.getByTestId('topic-index')).toHaveTextContent('none yet');
  });
});

/** Idle provider controls, with the ones a test needs overridden. */
const control = (overrides: Partial<MainProviderControl> = {}): MainProviderControl => ({
  selecting: false,
  unconfirmed: false,
  switchBlocked: false,
  retryBlocked: false,
  notice: null,
  retryOffered: false,
  restartOffered: false,
  select: vi.fn(),
  retry: vi.fn(),
  restart: vi.fn(),
  ...overrides
});

const withProvider = (provider: MainProviderInfo | null) => mainThreadFixture({ provider });

describe('MainThreadHeader main provider row', () => {
  it('shows the desired provider, the confirmed one and the status as separate things', () => {
    view(withProvider(providerFixture({ desired_provider: 'codex', effective_provider: 'claude', status: 'pending' })), {
      provider: control({ switchBlocked: true })
    });
    expect(screen.getByLabelText('Main provider')).toHaveValue('codex');
    expect(screen.getByTestId('mt-provider-effective')).toHaveTextContent('effective: Claude');
    expect(screen.getByTestId('mt-provider-status')).toHaveTextContent('preparing');
  });

  it.each([
    ['not_started', providerFixture({ status: 'not_started', effective_provider: null }), 'not started, starts with your next message', 'none confirmed'],
    ['ready', providerFixture(), 'ready', 'Claude'],
    ['failed', providerFixture({ status: 'failed', effective_provider: null, error: 'Codex did not start.' }), 'failed', 'none confirmed']
  ])('shows the %s status in words, not colour alone', (_status, provider, words, effective) => {
    view(withProvider(provider), { provider: control() });
    expect(screen.getByTestId('mt-provider-status')).toHaveTextContent(words);
    expect(screen.getByTestId('mt-provider-effective')).toHaveTextContent(effective);
  });

  it('says so when the provider state is unknown, and the select is held', () => {
    view(withProvider(null), { provider: control({ switchBlocked: true }) });
    expect(screen.getByTestId('mt-provider-status')).toHaveTextContent('state unknown');
    expect(screen.getByTestId('mt-provider-select')).toBeDisabled();
    expect(screen.getByRole('option', { name: 'unknown' })).toBeInTheDocument();
  });

  it('holds the select while the control says the switch is blocked', () => {
    view(withProvider(providerFixture()), { provider: control({ switchBlocked: true }) });
    expect(screen.getByTestId('mt-provider-select')).toBeDisabled();
  });

  it('holds Retry by its own rule, so a pending switch can offer it with the select held', () => {
    view(
      withProvider(providerFixture({ desired_provider: 'codex', effective_provider: null, status: 'pending' })),
      { provider: control({ switchBlocked: true, retryOffered: true, retryBlocked: false }) }
    );
    expect(screen.getByTestId('mt-provider-select')).toBeDisabled();
    expect(screen.getByTestId('mt-provider-retry')).toBeEnabled();
  });

  it('holds Retry while its own rule says so', () => {
    view(withProvider(providerFixture()), { provider: control({ retryOffered: true, retryBlocked: true }) });
    expect(screen.getByTestId('mt-provider-retry')).toBeDisabled();
  });

  it('announces the provider status as a polite status region', () => {
    view(withProvider(providerFixture({ status: 'pending', effective_provider: null })), { provider: control() });
    expect(screen.getByTestId('mt-provider-status')).toHaveAttribute('role', 'status');
  });

  it('offers the other provider and reports the choice', async () => {
    const select = vi.fn();
    view(withProvider(providerFixture()), { provider: control({ select }) });
    await userEvent.selectOptions(screen.getByLabelText('Main provider'), 'codex');
    expect(select).toHaveBeenCalledWith('codex');
  });

  it('shows the state as offline and holds the select while the backend is unreachable', () => {
    view(withProvider(providerFixture()), { offline: true, provider: control({ switchBlocked: true }) });
    expect(screen.getByTestId('mt-provider-status')).toHaveTextContent('offline, selection unavailable');
    expect(screen.getByTestId('mt-provider-select')).toBeDisabled();
  });

  it('says when a selection is in flight', () => {
    view(withProvider(providerFixture()), { provider: control({ selecting: true, switchBlocked: true }) });
    expect(screen.getByTestId('mt-provider-status')).toHaveTextContent('selecting provider…');
  });

  it('says when the provider state is unconfirmed', () => {
    view(withProvider(providerFixture()), { provider: control({ unconfirmed: true, switchBlocked: true }) });
    expect(screen.getByTestId('mt-provider-status')).toHaveTextContent('provider state unconfirmed');
  });

  it('shows the bounded error of a failed start', () => {
    view(withProvider(providerFixture({ status: 'failed', error: 'Codex did not start.' })), {
      provider: control({ retryOffered: true })
    });
    expect(screen.getByTestId('mt-provider-error')).toHaveTextContent('Codex did not start.');
  });

  it('offers Retry only when the control offers it, and runs it on request', async () => {
    const retry = vi.fn();
    const { rerender } = view(withProvider(providerFixture()), { provider: control() });
    expect(screen.queryByTestId('mt-provider-retry')).toBeNull();
    rerender(
      <MainThreadHeader info={withProvider(providerFixture())} offline={false} provider={control({ retryOffered: true, retry })} />
    );
    await userEvent.click(screen.getByRole('button', { name: 'Retry' }));
    expect(retry).toHaveBeenCalledOnce();
  });

  it('offers Restart only when the control offers it, and runs it on request', async () => {
    const restart = vi.fn();
    view(withProvider(providerFixture({ status: 'failed', recoverable: true })), {
      provider: control({ restartOffered: true, restart, retryOffered: true })
    });
    await userEvent.click(screen.getByRole('button', { name: 'Restart' }));
    expect(restart).toHaveBeenCalledOnce();
  });

  it('shows the notice of a selection as an alert', () => {
    view(withProvider(providerFixture()), {
      provider: control({ notice: 'Provider change blocked (409): busy. State is refreshed; no automatic retry.' })
    });
    expect(screen.getByRole('alert')).toHaveTextContent('Provider change blocked (409)');
  });

  it('hides the row when no provider controls are given', () => {
    view(withProvider(providerFixture()));
    expect(screen.queryByTestId('mt-provider')).toBeNull();
  });

  it('explains what a switch keeps, under details', async () => {
    view(withProvider(providerFixture()), { provider: control() });
    await userEvent.click(screen.getByTestId('mt-details-toggle'));
    expect(screen.getByTestId('mt-provider-help')).toHaveTextContent('Conversation and tasks stay.');
  });
});
