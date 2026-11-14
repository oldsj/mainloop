import { describe, expect, it, vi } from 'vitest';
import { createSignIn, RELOAD_COOLDOWN_MS } from './signin';

function memoryStorage() {
  const items = new Map<string, string>();
  return {
    getItem: (key: string) => items.get(key) ?? null,
    setItem: (key: string, value: string) => void items.set(key, value)
  };
}

describe('createSignIn', () => {
  it('reloads once on a lapsed sign-in, so the gateway can send the owner through the login', () => {
    const now = 1_000_000;
    const reload = vi.fn();
    const signIn = createSignIn({ now: () => now, reload, storage: memoryStorage() });
    signIn.expired();
    expect(reload).toHaveBeenCalledTimes(1);
    expect(signIn.getSnapshot()).toBe('signed-in');
  });

  it('stops reloading when the 401 comes back right after a reload, and asks instead', () => {
    let now = 1_000_000;
    const storage = memoryStorage();
    const reload = vi.fn();
    // The page before the reload, then the page after it, sharing the session's storage.
    createSignIn({ now: () => now, reload, storage }).expired();
    now += 5_000;
    const after = createSignIn({ now: () => now, reload, storage });
    const listener = vi.fn();
    after.subscribe(listener);
    after.expired();
    after.expired();
    expect(reload).toHaveBeenCalledTimes(1);
    expect(after.getSnapshot()).toBe('expired');
    expect(listener).toHaveBeenCalledTimes(1);

    after.signInAgain();
    expect(reload).toHaveBeenCalledTimes(2);
  });

  it('reloads again once the cooldown has passed', () => {
    let now = 1_000_000;
    const storage = memoryStorage();
    const reload = vi.fn();
    createSignIn({ now: () => now, reload, storage }).expired();
    now += RELOAD_COOLDOWN_MS + 1;
    createSignIn({ now: () => now, reload, storage }).expired();
    expect(reload).toHaveBeenCalledTimes(2);
  });

  it('asks rather than reloading when it cannot remember a reload', () => {
    const reload = vi.fn();
    const signIn = createSignIn({ reload, storage: null });
    signIn.expired();
    expect(reload).not.toHaveBeenCalled();
    expect(signIn.getSnapshot()).toBe('expired');
  });
});
