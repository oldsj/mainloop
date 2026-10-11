import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { deliveryFixture, mainThreadFixture, nativeFixture, type MainThreadInfo } from './fixtures';
import { MainThreadHeader } from './MainThreadHeader';

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
