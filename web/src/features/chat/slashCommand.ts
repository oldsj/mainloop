/**
 * Recognizes a leading `/<name> ...args` token in composer text: a command name plus its
 * whitespace-separated arguments. Anything not starting with `/` is ordinary text.
 */
export interface ParsedSlashCommand {
  name: string;
  args: string[];
}

export function parseSlashCommand(text: string): ParsedSlashCommand | null {
  const trimmed = text.trim();
  if (!trimmed.startsWith('/')) return null;
  const [name, ...args] = trimmed.slice(1).split(/\s+/).filter(Boolean);
  if (!name) return null;
  return { name: name.toLowerCase(), args };
}

/** One slash command's entry in the autosuggest list: its name and a one-line summary. */
export interface SlashCommandSpec {
  name: string;
  summary: string;
}

/** Every slash command the composer offers, in the order the autosuggest list shows them. */
export const SLASH_COMMANDS: readonly SlashCommandSpec[] = [
  { name: 'model', summary: "Switch the main session's model provider" }
];

/** The commands in `registry` whose name starts with `query`, case-insensitively, in list order. */
export function matchSlashCommands(
  registry: readonly SlashCommandSpec[],
  query: string
): SlashCommandSpec[] {
  const lower = query.toLowerCase();
  return registry.filter((command) => command.name.startsWith(lower));
}

/** The command-name token the autosuggest dropdown tracks, and where a Tab-complete replaces it. */
export interface ActiveSlashToken {
  /** Characters typed after the leading `/`, as typed (not case-folded). */
  query: string;
  /** Always 0: a command only autosuggests at the very start of the box. */
  start: number;
  /** Index just past the token's last character, where a completion's replacement ends. */
  end: number;
}

/**
 * The command-name token under the caret, or null when the caret is not there to autosuggest
 * against: the text must start with `/` with no leading whitespace, and the caret must sit inside
 * the run of non-space characters right after it, not out in the arguments.
 */
export function activeSlashToken(text: string, caret: number): ActiveSlashToken | null {
  if (!text.startsWith('/')) return null;
  const name = /^\/(\S*)/.exec(text)?.[1] ?? '';
  const end = 1 + name.length;
  if (caret < 1 || caret > end) return null;
  return { query: name, start: 0, end };
}
