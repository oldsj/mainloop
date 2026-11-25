import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  approveInNativeShell,
  inNativeShell,
  openNativeDeviceSettings,
  sessionExpiredInNativeShell
} from './nativeShell';

type Handler = { postMessage: ReturnType<typeof vi.fn> };

function installShell(reply: (message: unknown) => Promise<unknown>): Handler {
  const handler = { postMessage: vi.fn(reply) };
  (window as unknown as { webkit: unknown }).webkit = { messageHandlers: { mainloop: handler } };
  return handler;
}

afterEach(() => {
  delete (window as unknown as { webkit?: unknown }).webkit;
});

const card = { itemId: 'q-1', actionId: 'pa-0001', digest: 'a'.repeat(64) };

describe('the iPhone app shell', () => {
  it('is absent in a browser, and approving there fails without approving', async () => {
    expect(inNativeShell()).toBe(false);
    expect(await approveInNativeShell(card)).toEqual({
      status: 'failed',
      message: "The iPhone app didn't answer. Nothing was approved."
    });
  });

  it('names the card to the app and takes its answer', async () => {
    const shell = installShell(async () => ({ status: 'approved' }));
    expect(inNativeShell()).toBe(true);
    expect(await approveInNativeShell(card)).toEqual({ status: 'approved' });
    expect(shell.postMessage).toHaveBeenCalledWith({
      type: 'approve',
      item_id: 'q-1',
      action_id: 'pa-0001',
      digest: card.digest
    });
  });

  it.each([
    ['a cancelled Touch ID prompt', { status: 'cancelled' }, { status: 'cancelled' }],
    [
      "the app's failure",
      { status: 'failed', message: 'The record does not match this card.' },
      { status: 'failed', message: 'The record does not match this card.' }
    ],
    [
      'an unknown status',
      { status: 'APPROVED' },
      { status: 'failed', message: "The iPhone app didn't answer. Nothing was approved." }
    ],
    [
      'no reply',
      undefined,
      { status: 'failed', message: "The iPhone app didn't answer. Nothing was approved." }
    ]
  ])('reads %s at the boundary', async (_name, reply, want) => {
    installShell(async () => reply);
    expect(await approveInNativeShell(card)).toEqual(want);
  });

  it('reads a thrown error as a failure', async () => {
    installShell(async () => {
      throw new Error('Touch ID is not available.');
    });
    expect(await approveInNativeShell(card)).toEqual({
      status: 'failed',
      message: 'Touch ID is not available.'
    });
  });

  it('asks the app for a new session and for its own settings', () => {
    const shell = installShell(async () => undefined);
    sessionExpiredInNativeShell();
    openNativeDeviceSettings();
    expect(shell.postMessage.mock.calls).toEqual([
      [{ type: 'sessionExpired' }],
      [{ type: 'openDeviceSettings' }]
    ]);
  });
});
