/**
 * Pure rules for the scheduled runs UI: the cron preview, the config limits, the time zone
 * defaults, the agent choices, and the idempotency keys a create or trigger reuses on retry. Free of
 * React and the network, so each rule has a unit test on its own.
 */
import { z } from 'zod';
import { ApiError, type components } from '@/api';
import type { Tone } from '@/lib/taskState';

type Schemas = components['schemas'];
export type ScheduleView = Schemas['ScheduledRunOutput'];
export type ScheduleConfigInput = Schemas['ScheduledRunConfigInput'];
export type ScheduleConfigOutput = Schemas['ScheduledRunConfigOutput'];
export type ScheduleAgentRef = Schemas['ScheduledRunAgentRef'];
export type ScheduleExecution = Schemas['ScheduledRunExecutionOutput'];
export type ProviderProfile = Schemas['ProviderProfile'];

/** Contract limits, from `ScheduledRunConfigInput` and `ScheduledRunCreate`. */
export const PROMPT_MAX_BYTES = 32_768;
export const MIN_TIMEOUT_SECONDS = 0.000001;
export const MAX_TIMEOUT_SECONDS = 9_223_372_036;
export const DEFAULT_TIMEOUT_SECONDS = 900;

/** The form's text. Numbers stay strings until they pass validation. */
export interface ScheduleDraft {
  name: string;
  /** The agent's `namespace/name` value, set on create only. */
  agent: string;
  schedule: string;
  timeZone: string;
  prompt: string;
  paused: boolean;
  timeoutSeconds: string;
}

export function emptyDraft(browserZone: string = browserTimeZone()): ScheduleDraft {
  return {
    name: '',
    agent: '',
    schedule: '0 * * * *',
    timeZone: browserZone,
    prompt: '',
    paused: false,
    timeoutSeconds: String(DEFAULT_TIMEOUT_SECONDS)
  };
}

export function draftFromConfig(config: ScheduleConfigOutput): ScheduleDraft {
  return {
    name: config.name,
    agent: '',
    schedule: config.schedule,
    timeZone: config.time_zone,
    prompt: config.prompt,
    paused: config.paused,
    timeoutSeconds: String(config.execution_timeout_seconds)
  };
}

/* ---------------------------------------------------------------- cron */

const MONTH_NAMES = [
  'January',
  'February',
  'March',
  'April',
  'May',
  'June',
  'July',
  'August',
  'September',
  'October',
  'November',
  'December'
];
const WEEKDAY_NAMES = [
  'Sunday',
  'Monday',
  'Tuesday',
  'Wednesday',
  'Thursday',
  'Friday',
  'Saturday'
];

interface FieldSpec {
  label: string;
  min: number;
  max: number;
  /** Three-letter names the scheduler also accepts, in value order from `min`. */
  names?: readonly string[];
}

// The same bounds as robfig/cron v3, which the kagent fork uses (day of week is 0-6, Sunday first).
const FIELDS: readonly FieldSpec[] = [
  { label: 'minute', min: 0, max: 59 },
  { label: 'hour', min: 0, max: 23 },
  { label: 'day of month', min: 1, max: 31 },
  { label: 'month', min: 1, max: 12, names: MONTH_NAMES.map((m) => m.slice(0, 3).toLowerCase()) },
  {
    label: 'day of week',
    min: 0,
    max: 6,
    names: WEEKDAY_NAMES.map((d) => d.slice(0, 3).toLowerCase())
  }
];

type FieldRead =
  | { kind: 'any' }
  | { kind: 'every'; step: number }
  | { kind: 'single'; value: number }
  | { kind: 'range'; from: number; to: number }
  | { kind: 'other'; text: string }
  | { kind: 'error'; message: string };

export type CronPreview = { ok: true; text: string } | { ok: false; message: string };

function valueOf(token: string, spec: FieldSpec): number | null {
  if (/^\d+$/.test(token)) return Number(token);
  const index = spec.names?.indexOf(token.toLowerCase()) ?? -1;
  return index >= 0 ? index + spec.min : null;
}

/** The first problem with one field, or null when every part of it is valid. */
function fieldError(raw: string, spec: FieldSpec): string | null {
  const bounds = spec.names ? `${spec.min}–${spec.max} or a name` : `${spec.min}–${spec.max}`;
  for (const part of raw.split(',')) {
    const match = /^(\*|[a-z0-9]+(?:-[a-z0-9]+)?)(?:\/(\d+))?$/i.exec(part);
    if (!match) return `The ${spec.label} field "${raw}" is not valid.`;
    const [, base = '', step] = match;
    if (step !== undefined && Number(step) < 1) return `The ${spec.label} step must be at least 1.`;
    if (base === '*') continue;
    const [fromToken = '', toToken] = base.split('-');
    const from = valueOf(fromToken, spec);
    if (from === null || from < spec.min || from > spec.max) {
      return `The ${spec.label} must be ${bounds}.`;
    }
    if (toToken === undefined) continue;
    const to = valueOf(toToken, spec);
    if (to === null || to < spec.min || to > spec.max) {
      return `The ${spec.label} must be ${bounds}.`;
    }
    if (to < from) return `The ${spec.label} range runs backwards.`;
  }
  return null;
}

function readField(raw: string, spec: FieldSpec): FieldRead {
  const message = fieldError(raw, spec);
  if (message) return { kind: 'error', message };
  if (raw === '*') return { kind: 'any' };
  const every = /^\*\/(\d+)$/.exec(raw);
  if (every) return { kind: 'every', step: Number(every[1]) };
  if (/^[^,/-]+$/.test(raw)) return { kind: 'single', value: valueOf(raw, spec) as number };
  const range = /^([^,/-]+)-([^,/-]+)$/.exec(raw);
  if (range) {
    const [, fromText = '', toText = ''] = range;
    return {
      kind: 'range',
      from: valueOf(fromText, spec) as number,
      to: valueOf(toText, spec) as number
    };
  }
  return { kind: 'other', text: raw };
}

const pad = (n: number) => String(n).padStart(2, '0');

/**
 * A field as plain words for the fallback sentence. `labels` names the values, starting at
 * `first` (months start at 1, weekdays at 0).
 */
function words(read: FieldRead, labels?: readonly string[], first = 0): string {
  const label = (value: number) => labels?.[value - first] ?? String(value);
  switch (read.kind) {
    case 'any':
      return 'any';
    case 'every':
      return `every ${read.step}`;
    case 'single':
      return label(read.value);
    case 'range':
      return `${label(read.from)} to ${label(read.to)}`;
    case 'other':
      return read.text;
    case 'error':
      return '';
  }
}

/**
 * Says in plain words when a five-field cron expression runs, or why it cannot. Covers the common
 * shapes exactly; any other valid expression gets a field-by-field sentence rather than a guess.
 * The server stays the authority: this only shows what the scheduler will read.
 */
export function describeCron(expression: string): CronPreview {
  const trimmed = expression.trim();
  if (trimmed === '') return { ok: false, message: 'Enter a five-field schedule.' };
  if (trimmed.startsWith('@')) {
    return { ok: false, message: 'Macros such as @daily are not supported. Use five fields.' };
  }
  const parts = trimmed.split(/\s+/);
  if (parts.length !== 5) {
    return {
      ok: false,
      message: `Use five fields: minute, hour, day of month, month, day of week. Found ${parts.length}.`
    };
  }
  const reads = FIELDS.map((spec, i) => readField(parts[i] ?? '', spec));
  const failed = reads.find((read) => read.kind === 'error');
  if (failed && failed.kind === 'error') return { ok: false, message: failed.message };

  const [minute, hour, dom, month, dow] = reads as [
    FieldRead,
    FieldRead,
    FieldRead,
    FieldRead,
    FieldRead
  ];
  const dayOnly = dom.kind === 'any' && month.kind === 'any' && dow.kind === 'any';
  const yearly = dom.kind === 'single' && month.kind === 'single' && dow.kind === 'any';
  const monthly = dom.kind === 'single' && month.kind === 'any' && dow.kind === 'any';
  const weekly = dom.kind === 'any' && month.kind === 'any';
  // A fixed minute and hour, with the day fields as the caller set them.
  const time =
    minute.kind === 'single' && hour.kind === 'single'
      ? `${pad(hour.value)}:${pad(minute.value)}`
      : null;

  if (dayOnly) {
    if (minute.kind === 'any' && hour.kind === 'any') return { ok: true, text: 'Every minute' };
    if (minute.kind === 'every' && hour.kind === 'any') {
      return {
        ok: true,
        text: minute.step === 1 ? 'Every minute' : `Every ${minute.step} minutes`
      };
    }
    if (minute.kind === 'single' && hour.kind === 'any') {
      return { ok: true, text: `Every hour, at minute ${minute.value}` };
    }
    if (minute.kind === 'single' && hour.kind === 'every') {
      return {
        ok: true,
        text:
          hour.step === 1
            ? `Every hour, at minute ${minute.value}`
            : `Every ${hour.step} hours, at minute ${minute.value}`
      };
    }
  }

  if (time && weekly) {
    if (dow.kind === 'single') {
      return { ok: true, text: `Every ${WEEKDAY_NAMES[dow.value]} at ${time}` };
    }
    if (dow.kind === 'range' && dow.from === 1 && dow.to === 5) {
      return { ok: true, text: `Every weekday (Monday to Friday) at ${time}` };
    }
  }

  if (time && dayOnly) return { ok: true, text: `Every day at ${time}` };

  if (time && monthly && dom.kind === 'single') {
    return { ok: true, text: `On day ${dom.value} of every month at ${time}` };
  }

  if (time && yearly && dom.kind === 'single' && month.kind === 'single') {
    return {
      ok: true,
      text: `Every year on ${MONTH_NAMES[month.value - 1]} ${dom.value} at ${time}`
    };
  }

  return {
    ok: true,
    text: `Custom: minute ${words(minute)}, hour ${words(hour)}, day of month ${words(dom, undefined, 1)}, month ${words(month, MONTH_NAMES, 1)}, day of week ${words(dow, WEEKDAY_NAMES)}`
  };
}

/* ------------------------------------------------------------ time zones */

/** IANA names only. "Local" is rejected: it would depend on the scheduler host. */
export function isValidTimeZone(zone: string): boolean {
  if (zone === 'Local') return false;
  try {
    new Intl.DateTimeFormat('en-US', { timeZone: zone });
    return true;
  } catch {
    return false;
  }
}

/** The browser's zone, or UTC when the browser does not report a usable one. */
export function browserTimeZone(): string {
  const zone = Intl.DateTimeFormat().resolvedOptions().timeZone;
  return zone && isValidTimeZone(zone) ? zone : 'UTC';
}

/** Zone names for the picker. An empty list is fine: the field still takes typed names. */
export function timeZoneOptions(): string[] {
  try {
    return Intl.supportedValuesOf('timeZone');
  } catch {
    return [];
  }
}

/** A run time in the schedule's own zone, so "02:00" means 02:00 where the schedule is set. */
export function formatInZone(iso: string | null, zone: string): string {
  if (!iso) return 'Not scheduled';
  try {
    return new Intl.DateTimeFormat(undefined, {
      timeZone: zone,
      dateStyle: 'medium',
      timeStyle: 'short'
    }).format(new Date(iso));
  } catch {
    return iso;
  }
}

/* ------------------------------------------------------------ validation */

const byteLength = (text: string) => new TextEncoder().encode(text).length;

/** A blank zone means UTC, as the contract says. Everything else is trimmed. */
export function normalizeTimeZone(zone: string): string {
  const trimmed = zone.trim();
  return trimmed === '' ? 'UTC' : trimmed;
}

const configSchema = z.object({
  name: z.string().max(200, 'Use 200 characters or fewer.'),
  schedule: z
    .string()
    .max(256, 'Use 256 characters or fewer.')
    .refine((value) => describeCron(value).ok, { error: 'Enter a valid five-field schedule.' }),
  time_zone: z
    .string()
    .max(253, 'Use 253 characters or fewer.')
    .refine(isValidTimeZone, { error: 'Choose an IANA time zone, such as Europe/London.' }),
  prompt: z
    .string()
    .min(1, 'Enter a prompt.')
    .refine((value) => value.trim() !== '', { error: 'Enter a prompt.' })
    .refine((value) => byteLength(value) <= PROMPT_MAX_BYTES, {
      error: `The prompt is over ${PROMPT_MAX_BYTES} bytes.`
    }),
  paused: z.boolean(),
  execution_timeout_seconds: z
    .number({ error: 'Enter a number of seconds.' })
    .min(MIN_TIMEOUT_SECONDS, `The timeout must be at least ${MIN_TIMEOUT_SECONDS} seconds.`)
    .max(MAX_TIMEOUT_SECONDS, 'The timeout is too large.')
}) satisfies z.ZodType<ScheduleConfigInput>;

export type DraftErrors = Partial<Record<keyof ScheduleConfigInput, string>>;

/**
 * Checks the draft against the contract limits. On success the config is ready to send; on failure
 * each message is keyed by the contract's field name, so the form can place it.
 */
export function validateDraft(
  draft: ScheduleDraft
): { ok: true; config: ScheduleConfigInput } | { ok: false; errors: DraftErrors } {
  const candidate = {
    name: draft.name.trim(),
    schedule: draft.schedule.trim().split(/\s+/).join(' '),
    time_zone: normalizeTimeZone(draft.timeZone),
    prompt: draft.prompt,
    paused: draft.paused,
    execution_timeout_seconds:
      draft.timeoutSeconds.trim() === '' ? NaN : Number(draft.timeoutSeconds)
  };
  const parsed = configSchema.safeParse(candidate);
  if (parsed.success) return { ok: true, config: parsed.data };

  const errors: DraftErrors = {};
  for (const issue of parsed.error.issues) {
    const field = issue.path[0] as keyof ScheduleConfigInput | undefined;
    if (field && !errors[field]) errors[field] = issue.message;
  }
  return { ok: false, errors };
}

/* ----------------------------------------------------------- agents */

export interface AgentOption {
  /** `namespace/name`, the select's value. */
  value: string;
  label: string;
  ref: ScheduleAgentRef;
}

/**
 * The Agents a schedule can run on: every role of every enabled provider, once each. An Agent that
 * several providers share is listed once, under the first provider that names it.
 */
export function agentOptions(providers: ProviderProfile[] | undefined): AgentOption[] {
  const options = new Map<string, AgentOption>();
  for (const provider of providers ?? []) {
    if (!provider.enabled) continue;
    for (const [role, ref] of Object.entries(provider.agents)) {
      const value = agentText(ref);
      if (options.has(value)) continue;
      options.set(value, {
        value,
        label: `${provider.display_name}, ${role}: ${ref.name}`,
        ref: { namespace: ref.namespace, name: ref.name }
      });
    }
  }
  return [...options.values()];
}

/* ----------------------------------------------------------- status */

const STATE_LABELS: Record<ScheduleExecution['state'], string> = {
  pending: 'Pending',
  running: 'Running',
  succeeded: 'Succeeded',
  failed: 'Failed',
  timed_out: 'Timed out'
};

const STATE_TONES: Record<ScheduleExecution['state'], Tone> = {
  pending: 'active',
  running: 'active',
  succeeded: 'ok',
  failed: 'bad',
  timed_out: 'bad'
};

export function executionStateLabel(state: string): string {
  return STATE_LABELS[state as ScheduleExecution['state']] ?? state;
}

export function executionStateTone(state: string): Tone {
  return STATE_TONES[state as ScheduleExecution['state']] ?? 'muted';
}

/** The HTTP status behind a failed call, or null when the call never got a reply. */
export function statusOf(error: unknown): number | null {
  return error instanceof ApiError ? error.status : null;
}

/** The server's message for a failed call, in words the owner can read. */
export function messageOf(error: unknown, fallback = 'The request failed.'): string {
  return error instanceof Error && error.message ? error.message : fallback;
}

/** An Agent as `namespace/name`, the form the list and the picker both show. */
export function agentText(ref: ScheduleAgentRef): string {
  return `${ref.namespace}/${ref.name}`;
}

/** An etag as the If-Match header wants it: quoted. */
export const ifMatch = (etag: string) => `"${etag}"`;

/* ------------------------------------------------------ idempotency */

/**
 * One request id per attempt. A retry of the same request sends the same id, so the server returns
 * the work it already did instead of doing it twice; a changed request gets a new id. `reset` ends
 * the attempt once it has succeeded or the server has said the id is spent.
 */
export function createRequestIds(mint: () => string = () => crypto.randomUUID()) {
  let held: { fingerprint: string; id: string } | null = null;
  return {
    idFor(fingerprint: string): string {
      if (held?.fingerprint !== fingerprint) held = { fingerprint, id: mint() };
      return held.id;
    },
    reset() {
      held = null;
    }
  };
}
