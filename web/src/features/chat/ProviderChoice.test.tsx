import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { mainThreadFixture, nativeFixture, providerFixture } from './fixtures';
import type { MainProviderControl } from './mainProvider';
import { ChatControls, ProviderChoice } from './ProviderChoice';
import type { MainProviderInfo } from './types';

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

const thread = (provider: MainProviderInfo | null = providerFixture()) =>
  mainThreadFixture({ provider });

const option = (name: string) => screen.getByRole('radio', { name });

describe('ProviderChoice', () => {
  it('marks the selected provider', () => {
    render(<ProviderChoice info={thread()} provider={control()} />);
    expect(
      within(screen.getByRole('radiogroup', { name: 'Main provider' })).getAllByRole('radio')
    ).toHaveLength(2);
    expect(option('Claude')).toBeChecked();
    expect(option('Codex')).not.toBeChecked();
  });

  it('asks before switching, and sends nothing until the owner confirms', async () => {
    const select = vi.fn();
    const onDone = vi.fn();
    render(<ProviderChoice info={thread()} provider={control({ select })} onDone={onDone} />);
    await userEvent.click(option('Codex'));
    const confirm = screen.getByRole('group', { name: 'Switch to Codex' });
    expect(confirm).toHaveTextContent('starts a fresh native session');
    expect(confirm).toHaveTextContent('conversation, tasks and earlier native history are kept');
    expect(select).not.toHaveBeenCalled();
    await userEvent.click(within(confirm).getByRole('button', { name: 'Switch to Codex' }));
    expect(select).toHaveBeenCalledExactlyOnceWith('codex');
    expect(onDone).toHaveBeenCalledOnce();
    expect(screen.queryByTestId('provider-confirm')).toBeNull();
  });

  it('cancels without sending anything', async () => {
    const select = vi.fn();
    render(<ProviderChoice info={thread()} provider={control({ select })} />);
    await userEvent.click(option('Codex'));
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByTestId('provider-confirm')).toBeNull();
    expect(select).not.toHaveBeenCalled();
  });

  it('asks nothing when the ready provider is chosen again', async () => {
    const select = vi.fn();
    render(<ProviderChoice info={thread()} provider={control({ select })} />);
    await userEvent.click(option('Claude'));
    expect(screen.queryByTestId('provider-confirm')).toBeNull();
    expect(select).not.toHaveBeenCalled();
  });

  it('offers the selected provider again when it is not ready, so a failed start can be re-chosen', async () => {
    const select = vi.fn();
    const failed = providerFixture({ status: 'failed', effective_provider: null, error: 'x' });
    render(<ProviderChoice info={thread(failed)} provider={control({ select })} />);
    await userEvent.click(option('Claude'));
    await userEvent.click(screen.getByTestId('provider-confirm-switch'));
    expect(select).toHaveBeenCalledExactlyOnceWith('claude');
  });

  it('holds both options and says why while a switch is blocked', async () => {
    const select = vi.fn();
    render(<ProviderChoice info={thread()} provider={control({ select, switchBlocked: true })} />);
    expect(option('Claude')).toBeDisabled();
    expect(option('Codex')).toBeDisabled();
    expect(screen.getByTestId('provider-held')).toHaveTextContent('cannot change right now');
    await userEvent.click(option('Codex'));
    expect(screen.queryByTestId('provider-confirm')).toBeNull();
    expect(select).not.toHaveBeenCalled();
  });

  it('drops an open confirmation when the switch becomes blocked', async () => {
    const select = vi.fn();
    const { rerender } = render(<ProviderChoice info={thread()} provider={control({ select })} />);
    await userEvent.click(option('Codex'));
    rerender(
      <ProviderChoice info={thread()} provider={control({ select, switchBlocked: true })} />
    );
    expect(screen.queryByTestId('provider-confirm')).toBeNull();
  });

  it('checks neither option when the provider state is unknown', () => {
    render(<ProviderChoice info={thread(null)} provider={control({ switchBlocked: true })} />);
    expect(option('Claude')).not.toBeChecked();
    expect(option('Codex')).not.toBeChecked();
  });
});

describe('ChatControls', () => {
  it('names the provider and the model in use, without the vendor prefix', () => {
    render(<ChatControls info={thread()} provider={control()} />);
    expect(screen.getByTestId('session-controls')).toHaveTextContent('Claude · sonnet-5-5');
  });

  it('names the provider alone when the session reports no model', () => {
    const info = mainThreadFixture({
      provider: providerFixture(),
      native: nativeFixture({ model: null, kind: 'claude' })
    });
    render(<ChatControls info={info} provider={control()} />);
    expect(screen.getByTestId('session-controls')).toHaveTextContent(/^Claude\s*▾$/);
  });

  it('says so when the provider is unknown', () => {
    render(<ChatControls info={thread(null)} provider={control({ switchBlocked: true })} />);
    expect(screen.getByTestId('session-controls')).toHaveTextContent('provider unknown');
  });

  it('opens one picker with the provider choice, and Escape closes it and returns focus', async () => {
    render(<ChatControls info={thread()} provider={control()} />);
    const chip = screen.getByTestId('session-controls');
    expect(chip).toHaveAttribute('aria-expanded', 'false');
    await userEvent.click(chip);
    const picker = screen.getByRole('dialog', { name: 'Session controls' });
    expect(within(picker).getByRole('radiogroup', { name: 'Main provider' })).toBeInTheDocument();
    expect(picker).toHaveTextContent('/model');
    await userEvent.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(chip).toHaveFocus();
  });

  it('closes the picker once a switch is confirmed', async () => {
    const select = vi.fn();
    render(<ChatControls info={thread()} provider={control({ select })} />);
    await userEvent.click(screen.getByTestId('session-controls'));
    await userEvent.click(option('Codex'));
    await userEvent.click(screen.getByTestId('provider-confirm-switch'));
    expect(select).toHaveBeenCalledExactlyOnceWith('codex');
    expect(screen.queryByRole('dialog')).toBeNull();
  });
});
