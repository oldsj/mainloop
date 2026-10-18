/**
 * Recognizes a leading `/<name> ...args` token in composer text. There is no other
 * slash-command infrastructure in the app to extend, so this is deliberately minimal: a command
 * name plus its whitespace-separated arguments. Anything not starting with `/` is ordinary text.
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
