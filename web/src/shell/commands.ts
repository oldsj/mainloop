/**
 * The shell's own actions, by name, so they can be run from more than one place: the prefix keys
 * (Ctrl+B, then a key) and the command palette, which every browser passes to the page. The shell
 * that is on screen provides them; with none mounted a command does nothing.
 */
import { useEffect, useRef } from 'react';

export type ShellCommand = 'next-tab' | 'previous-tab' | 'new' | 'sidebar' | 'spaces';

/** The key after the prefix that runs each command, for the palette's hint and the shell's handler. */
export const COMMAND_KEYS: Record<ShellCommand, string> = {
  'next-tab': 'n',
  'previous-tab': 'p',
  new: 'c',
  sidebar: 'b',
  spaces: 'w'
};

let provider: ((command: ShellCommand) => void) | null = null;

/** Called by the shell on screen. Returns the function that withdraws it. */
export function provideShellCommands(run: (command: ShellCommand) => void): () => void {
  provider = run;
  return () => {
    if (provider === run) provider = null;
  };
}

/** Runs a command on the shell that is on screen. False when there is none to run it. */
export function runShellCommand(command: ShellCommand): boolean {
  if (!provider) return false;
  provider(command);
  return true;
}

/**
 * Provides the commands from a shell component for as long as it is mounted. `run` may change on
 * every render; the latest one is the one that runs.
 */
export function useShellCommands(run: (command: ShellCommand) => void): void {
  const latest = useRef(run);
  useEffect(() => {
    latest.current = run;
  }, [run]);
  useEffect(() => provideShellCommands((command) => latest.current(command)), []);
}
