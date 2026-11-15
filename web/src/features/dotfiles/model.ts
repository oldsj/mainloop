/**
 * The agent dotfiles source (docs/specs/agent-dotfiles.md): a GitHub repository, a ref and a
 * directory whose AGENTS.md, skills, subagents and commands agent sessions receive as native
 * user-level files. Validated at the boundary against the contract's shape.
 */
import { z } from 'zod';
import type { components } from '@/api';

export type DotfilesSource = components['schemas']['DotfilesSource'];

export const dotfilesSourceSchema = z.object({
  repository: z.string().nullable(),
  ref: z.string(),
  path: z.string(),
  version: z.number().int().nonnegative()
});

/** The contract's `owner/name` pattern, checked before a save so the owner sees why. */
export const REPOSITORY = /^[A-Za-z0-9][A-Za-z0-9-]{0,38}\/[A-Za-z0-9._-]{1,100}$/;

/** One line naming a set source, or null when none is set. */
export function sourceText(source: z.output<typeof dotfilesSourceSchema>): string | null {
  if (!source.repository) return null;
  const ref = source.ref || 'default branch';
  return source.path
    ? `${source.repository} @ ${ref}, ${source.path}/`
    : `${source.repository} @ ${ref}`;
}

/** The source's tree on GitHub, built from validated parts only. */
export function sourceUrl(source: z.output<typeof dotfilesSourceSchema>): string | null {
  if (!source.repository || !REPOSITORY.test(source.repository)) return null;
  const base = `https://github.com/${source.repository}`;
  if (!source.ref && !source.path) return base;
  const ref = encodeURIComponent(source.ref || 'HEAD');
  const path = source.path.split('/').filter(Boolean).map(encodeURIComponent).join('/');
  return path ? `${base}/tree/${ref}/${path}` : `${base}/tree/${ref}`;
}

/** The dotfiles a session's native Session was created with. */
export type SessionDotfiles = components['schemas']['SessionDotfiles'];

const REASONS: Record<string, string> = {
  app_not_installed: 'the GitHub App cannot read the repository',
  ref_not_found: 'the ref was not found',
  repository_unavailable: 'GitHub could not be read',
  over_limits: 'over the size limits',
  unsupported_file: 'holds a symlink or another unsupported file',
  nothing_to_export: 'no AGENTS.md, CLAUDE.md, skills, agents or commands there',
  secret_found: 'a possible secret was found'
};

/** One line for a session's pinned dotfiles: the repository and the short commit, or why none. */
export function pinText(pin: SessionDotfiles): string {
  if (pin.status === 'none') return 'none';
  const at = pin.commit ? ` @ ${pin.commit.slice(0, 7)}` : '';
  const where = `${pin.repository}${at}${pin.path ? `, ${pin.path}/` : ''}`;
  if (pin.status === 'ready') return where;
  const why = REASONS[pin.reason] ?? pin.reason;
  return pin.status === 'refused' ? `${where}: refused, ${why}` : `${where}: not read, ${why}`;
}

/** The pinned commit's directory on GitHub, or null without a commit. */
export function pinUrl(pin: SessionDotfiles): string | null {
  if (!pin.commit) return null;
  return sourceUrl({ repository: pin.repository, ref: pin.commit, path: pin.path, version: 0 });
}
