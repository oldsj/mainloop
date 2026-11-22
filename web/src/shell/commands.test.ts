import { renderHook } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { provideShellCommands, runShellCommand, useShellCommands } from './commands';

describe('shell commands', () => {
  it('does nothing, and says so, when no shell is on screen', () => {
    expect(runShellCommand('next-tab')).toBe(false);
  });

  it('runs a command on the shell that provides them, until it withdraws', () => {
    const run = vi.fn();
    const withdraw = provideShellCommands(run);

    expect(runShellCommand('sidebar')).toBe(true);
    expect(run).toHaveBeenCalledWith('sidebar');

    withdraw();
    expect(runShellCommand('sidebar')).toBe(false);
    expect(run).toHaveBeenCalledTimes(1);
  });

  it('does not withdraw a later shell’s commands when an earlier one unmounts', () => {
    const first = vi.fn();
    const second = vi.fn();
    const withdrawFirst = provideShellCommands(first);
    const withdrawSecond = provideShellCommands(second);

    withdrawFirst();
    runShellCommand('new');

    expect(second).toHaveBeenCalledWith('new');
    expect(first).not.toHaveBeenCalled();
    withdrawSecond();
  });

  it('provides a component’s latest handler for as long as it is mounted', () => {
    const before = vi.fn();
    const after = vi.fn();
    const { rerender, unmount } = renderHook(({ run }) => useShellCommands(run), {
      initialProps: { run: before }
    });

    rerender({ run: after });
    runShellCommand('spaces');
    expect(after).toHaveBeenCalledWith('spaces');
    expect(before).not.toHaveBeenCalled();

    unmount();
    expect(runShellCommand('spaces')).toBe(false);
  });
});
