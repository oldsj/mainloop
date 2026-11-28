/**
 * Owner memory (docs/specs/owner-memory.md): the owner's own standing preferences, loaded into
 * every session they have. Validated at the boundary against the contract's shape; the lists and
 * wording the Settings section shows are pure functions here, so each has a unit test.
 */
import { z } from 'zod';
import type { components } from '@/api';
import { contractEnum } from '@/lib/contractEnum';
import { keepValid, memorySourceSchema } from '@/features/projects/model';

type Schemas = components['schemas'];
export type OwnerMemoryEntry = Schemas['OwnerMemoryEntry'];
export type OwnerMemoryDecision = Schemas['OwnerMemoryDecision']['decision'];

const text = z.string();
const OWNER_MEMORY_STATES = contractEnum<Schemas['OwnerMemoryState']>()([
  'proposed',
  'active',
  'lapsed',
  'rejected',
  'withdrawn',
  'replaced',
  'forgotten',
  'moved'
]);
const OWNER_MEMORY_AUTHORS = contractEnum<OwnerMemoryEntry['author']>()(['main', 'task']);

export const ownerMemoryEntrySchema: z.ZodType<OwnerMemoryEntry> = z.object({
  id: text,
  text,
  state: z.enum(OWNER_MEMORY_STATES),
  author: z.enum(OWNER_MEMORY_AUTHORS),
  source: memorySourceSchema,
  replaces_id: text.nullable(),
  created_at: text,
  last_confirmed_at: text.nullable(),
  decided_at: text.nullable(),
  due_at: text.nullable(),
  lapsed_at: text.nullable(),
  in_dotfiles_commit: text.nullable(),
  moved_to_project_id: text.nullable(),
  moved_to_entry_id: text.nullable()
});

/** The totals and settings must parse; a malformed entry is dropped rather than failing the section. */
export const ownerMemorySchema = z.object({
  entries: z.array(z.unknown()),
  used_bytes: z.number().int().nonnegative(),
  max_bytes: z.number().int().positive(),
  max_entry_bytes: z.number().int().positive(),
  review_after_days: z.number().int().positive(),
  grace_days: z.number().int().nonnegative()
});

export const MIN_REVIEW_DAYS = 1;
export const MAX_REVIEW_DAYS = 365;

/** Whether an active entry's review window has run out by `now`. */
export function isDue(entry: OwnerMemoryEntry, now: number): boolean {
  if (entry.state !== 'active' || !entry.due_at) return false;
  const due = Date.parse(entry.due_at);
  return Number.isFinite(due) && due <= now;
}

const confirmedTime = (entry: OwnerMemoryEntry) =>
  entry.last_confirmed_at ? Date.parse(entry.last_confirmed_at) : Number.POSITIVE_INFINITY;

/**
 * The section's three lists. Proposals and active entries keep the server's order (oldest first);
 * the review queue (due and lapsed entries) is oldest confirmation first. A due entry is still
 * loaded, so it is in both the review queue and the active list.
 */
export function ownerMemoryLists(entries: unknown[], now: number) {
  const valid = keepValid(entries, ownerMemoryEntrySchema);
  return {
    proposed: valid.filter((entry) => entry.state === 'proposed'),
    review: valid
      .filter((entry) => entry.state === 'lapsed' || isDue(entry, now))
      .sort((a, b) => confirmedTime(a) - confirmedTime(b)),
    active: valid.filter((entry) => entry.state === 'active')
  };
}

const DAY_MS = 24 * 60 * 60 * 1000;

/** Whole days from `iso` to `now`, never negative. */
function daysSince(iso: string, now: number): number {
  const at = Date.parse(iso);
  if (!Number.isFinite(at)) return 0;
  return Math.max(0, Math.floor((now - at) / DAY_MS));
}

/** An entry's age as the owner reads it: "confirmed 12 days ago". */
export function confirmedAge(entry: OwnerMemoryEntry, now: number): string | null {
  if (!entry.last_confirmed_at) return null;
  const days = daysSince(entry.last_confirmed_at, now);
  if (days === 0) return 'confirmed today';
  return days === 1 ? 'confirmed 1 day ago' : `confirmed ${days} days ago`;
}

/** The date part of a timestamp (UTC), as the agents are given it. */
export function dateOnly(iso: string): string {
  return iso.slice(0, 10);
}

/** Who recorded an entry and when: main's are active at once, a task's are proposals. */
export function ownerMemoryOrigin(entry: OwnerMemoryEntry, ago: (iso: string) => string): string {
  const who = entry.author === 'main' ? 'By main' : 'Proposed by a task';
  return `${who}, ${ago(entry.created_at)}`;
}

/** The commit an entry was found at, short as the dotfiles views show commits. */
export function shortCommit(commit: string): string {
  return commit.slice(0, 7);
}

export const OWNER_MEMORY_EXPLANATION =
  "Your own standing preferences, true across every project, added to every agent session you have. The main session's entries apply at once; a supervisor's wait for you. An entry not confirmed within the review window comes up for review, and stops loading a week later unless you keep it.";
