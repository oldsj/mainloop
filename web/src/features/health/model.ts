/**
 * Pure rules for the health page: what the server's rows and observations must look like, how
 * rows sort, how unknown counts read, and what each filter choice offers. Kept free of React so
 * each rule has a unit test. A row or observation that fails its shape is dropped and counted,
 * so one bad record never takes down the list. A report or approval that fails its shape is left
 * out of its row and counted there.
 */
import { z } from 'zod';
import type { components } from '@/api';
import type { LoadFailure } from '@/features/sessions/model';
import { agentLabel } from '@/lib/agentLabel';
import type { HealthFilters } from '@/lib/contract';
import type { HealthSearch } from './search';

export type Project = components['schemas']['Project'];

export const COVERAGE_NOTE =
  'Totals use each task’s reported root-session usage. Product child tasks appear separately. Native child attribution, cache tokens, Codex usage and exact approval waits are unavailable.';
export const PARTIAL_NOTICE =
  'Collection is partial: this window exceeds the recovery or display limit. Filter to a session to inspect it.';
export const EMPTY_NOTICE =
  'No tasks or sessions match these filters. Try another window or clear the filters.';
export const OBSERVATIONS_PARTIAL = 'Observation list is partial.';
export const OBSERVATIONS_EMPTY = 'No retained source events in this window.';
export const UNASSIGNED = 'Unassigned';
/** At or above this width the filters start open. Matches the frozen page's breakpoint. */
export const WIDE_MIN_PX = 640;

const WINDOW_LABELS: Record<number, string> = {
  24: 'Last 24 hours',
  168: 'Last 7 days',
  720: 'Last 30 days',
  8760: 'Last year'
};
const WINDOW_SUMMARY: Record<number, string> = {
  24: 'last 24 hours',
  168: 'last 7 days',
  720: 'last 30 days',
  8760: 'last year'
};

export function windowLabel(hours: number): string {
  return WINDOW_LABELS[hours] ?? 'Last year';
}

export function windowSummary(hours: number): string {
  return WINDOW_SUMMARY[hours] ?? 'last year';
}

// The fields the page reads. Other fields pass through untouched.
const Totals = z.looseObject({
  turns: z.number(),
  usage_observed_turns: z.number(),
  usage_complete_turns: z.number(),
  input_tokens: z.number().nullable(),
  output_tokens: z.number().nullable(),
  failures: z.number().nullable(),
  repeated_failure_groups: z.number().nullable(),
  unresolved_calls: z.number(),
  coverage: z.enum(['complete', 'partial', 'unknown'])
});

const SessionShape = z.looseObject({
  session_id: z.string().nullable(),
  title: z.string().nullable().optional(),
  provider: z.string(),
  attempt_id: z.string().nullable(),
  totals: Totals
});

const ReportShape = z.looseObject({
  id: z.string(),
  task_id: z.string(),
  attempt_id: z.string(),
  outcome: z.enum(['blocked', 'failed'])
});

const ApprovalShape = z.looseObject({
  id: z.string(),
  runtime_session_id: z.string().nullable(),
  state: z.string().nullable()
});

// A row's own fields. Its reports and approvals are checked one at a time (see readClaims), so one
// bad claim costs that claim and not the row's failure totals.
const RowFields = z.looseObject({
  id: z.string(),
  task_id: z.string().nullable(),
  project_id: z.string().nullable(),
  title: z.string().nullable(),
  sessions: z.array(SessionShape),
  totals: Totals,
  gaps: z.array(z.string()),
  expected_receipts: z.number()
});

const RowShape = RowFields.extend({
  reports: z.array(z.unknown()),
  approvals: z.array(z.unknown())
});

const ViewShape = z.looseObject({
  rows: z.array(z.unknown()),
  hours: z.number(),
  partial: z.boolean(),
  retention: z.looseObject({ events_days: z.number(), summaries_days: z.number() })
});

export type HealthTotals = z.infer<typeof Totals>;
export type HealthSession = z.infer<typeof SessionShape>;
export type HealthReport = z.infer<typeof ReportShape>;
export type HealthApproval = z.infer<typeof ApprovalShape>;
export type HealthRow = z.infer<typeof RowFields> & {
  reports: HealthReport[];
  approvals: HealthApproval[];
  /** Reports and approvals on this row that could not be read. They are not listed, and the row says so. */
  unreadableClaims: number;
};

export interface HealthView {
  rows: HealthRow[];
  /** Rows that failed their shape. They are not listed, and the page says so. */
  dropped: number;
  /** Rows whose id repeats an earlier row. The first is listed; these are not, and the page says so. */
  repeated: number;
  partial: boolean;
  retention: { events_days: number; summaries_days: number };
}

/** Each claim is read on its own. A bad one is counted and left out, and the rest of the row still shows. */
function readClaims<T>(list: unknown[], shape: z.ZodType<T>): { items: T[]; unreadable: number } {
  const items: T[] = [];
  let unreadable = 0;
  for (const item of list) {
    const parsed = shape.safeParse(item);
    if (parsed.success) items.push(parsed.data);
    else unreadable += 1;
  }
  return { items, unreadable };
}

export function parseRows(list: unknown[]): {
  rows: HealthRow[];
  dropped: number;
  repeated: number;
} {
  const rows: HealthRow[] = [];
  const ids = new Set<string>();
  let dropped = 0;
  let repeated = 0;
  for (const item of list) {
    const parsed = RowShape.safeParse(item);
    if (!parsed.success) {
      dropped += 1;
      continue;
    }
    // Rows are keyed by id. A repeated id is not shown twice; it is counted apart from unreadable rows.
    if (ids.has(parsed.data.id)) {
      repeated += 1;
      continue;
    }
    ids.add(parsed.data.id);
    const reports = readClaims(parsed.data.reports, ReportShape);
    const approvals = readClaims(parsed.data.approvals, ApprovalShape);
    rows.push({
      ...parsed.data,
      reports: reports.items,
      approvals: approvals.items,
      unreadableClaims: reports.unreadable + approvals.unreadable
    });
  }
  return { rows, dropped, repeated };
}

/** The whole view, or null when the envelope itself is not readable. */
export function readView(value: unknown): HealthView | null {
  const parsed = ViewShape.safeParse(value);
  if (!parsed.success) return null;
  const { rows, dropped, repeated } = parseRows(parsed.data.rows);
  return {
    rows,
    dropped,
    repeated,
    partial: parsed.data.partial,
    retention: parsed.data.retention
  };
}

/** Most failures first. Unknown counts sort last, and the order is stable for ties. */
export function sortByFailures(rows: HealthRow[]): HealthRow[] {
  return [...rows].sort((a, b) => (b.totals.failures ?? -1) - (a.totals.failures ?? -1));
}

export function countText(value: number | null): string {
  return value === null ? 'Unavailable' : value.toLocaleString();
}

export function rowTitle(row: HealthRow): string {
  return row.title ?? row.task_id ?? UNASSIGNED;
}

export function coverageText(coverage: HealthTotals['coverage']): string {
  if (coverage === 'complete') return 'Observed coverage';
  if (coverage === 'partial') return 'Partial coverage';
  return 'Unknown coverage';
}

export function usageLine(row: HealthRow): string {
  const pending = row.approvals.filter((approval) => approval.state === 'pending').length;
  return `Usage observed on ${row.totals.usage_observed_turns}/${row.totals.turns} observed turns · ${row.expected_receipts} delivery receipts · ${row.reports.length} blocked/failed claims · ${pending} pending approvals`;
}

export function gapText(gaps: string[]): string {
  return `Collection gaps: ${gaps.map((gap) => gap.replaceAll('_', ' ')).join(', ')}.`;
}

export function reportText(report: HealthReport): string {
  return `Reported ${report.outcome} · attempt ${report.attempt_id} · report ${report.id}. Agent claim; runtime failures are counted separately.`;
}

export function approvalText(approval: HealthApproval): string {
  return `Approval ${approval.state ?? 'unknown'}; exact wait unavailable.`;
}

export function providerLabel(provider: string): string {
  return provider === 'claude' || provider === 'codex' ? agentLabel(provider) : provider;
}

export function sessionLine(session: HealthSession): string {
  const { totals } = session;
  return `${providerLabel(session.provider)} · input ${countText(totals.input_tokens)} · output ${countText(totals.output_tokens)} · failures ${countText(totals.failures)}`;
}

export function retentionText(retention: HealthView['retention']): string {
  return `Source detail retained ${retention.events_days} days; frozen, path-free summaries ${retention.summaries_days} days.`;
}

export function droppedRowsText(count: number): string {
  return count === 1
    ? 'One health row could not be read and is not listed.'
    : `${count} health rows could not be read and are not listed.`;
}

export function repeatedRowsText(count: number): string {
  return count === 1
    ? 'One health row repeats the id of a row above and is not listed again.'
    : `${count} health rows repeat the id of a row above and are not listed again.`;
}

export function unreadableClaimsText(count: number): string {
  return count === 1
    ? 'One report or approval on this row could not be read and is not listed.'
    : `${count} reports or approvals on this row could not be read and are not listed.`;
}

export function droppedObservationsText(count: number): string {
  return count === 1
    ? 'One observation could not be read and is not listed.'
    : `${count} observations could not be read and are not listed.`;
}

// Observations: the fields the page shows. The event kind is the discriminator.
const ToolShape = z.looseObject({
  tool_class: z.string(),
  outcome: z.string(),
  category: z.string(),
  path: z.string().nullable()
});

const EventRowShape = z.looseObject({
  event_id: z.string(),
  payload: z.looseObject({
    kind: z.enum(['turn', 'tool', 'usage']),
    sequence: z.number(),
    source: z.string(),
    tool: ToolShape.optional()
  })
});

const EventsShape = z.looseObject({
  events: z.array(z.unknown()),
  partial: z.boolean()
});

export type HealthEventRow = z.infer<typeof EventRowShape>;

export interface SourceObservations {
  events: HealthEventRow[];
  dropped: number;
  partial: boolean;
}

/** The source events for one session, or null when the envelope is not readable. */
export function readObservations(value: unknown): SourceObservations | null {
  const parsed = EventsShape.safeParse(value);
  if (!parsed.success) return null;
  const events: HealthEventRow[] = [];
  let dropped = 0;
  for (const item of parsed.data.events) {
    const event = EventRowShape.safeParse(item);
    if (event.success) events.push(event.data);
    else dropped += 1;
  }
  return { events, dropped, partial: parsed.data.partial };
}

/** One observation as the lines the page shows, in the frozen order. */
export function eventLines(event: HealthEventRow): string[] {
  const { payload } = event;
  const lines = [
    `${payload.kind} · sequence ${payload.sequence} · ${payload.source}`,
    event.event_id
  ];
  if (payload.tool) {
    const { tool } = payload;
    lines.push(
      `${tool.tool_class} · ${tool.outcome} · ${tool.category}${tool.path ? ` · ${tool.path}` : ''}`
    );
  }
  return lines;
}

/** The request filters. Empty values are left out of the query, as the frozen client did. */
export function healthQuery(filters: HealthFilters): HealthFilters {
  return Object.fromEntries(
    Object.entries(filters).filter(([, value]) => value !== '' && value != null)
  ) as HealthFilters;
}

/** The filters the URL holds, with the form's defaults filled in. */
export function filtersFrom(search: HealthSearch): HealthFilters {
  return {
    project_id: search.project_id ?? '',
    task_id: search.task_id ?? '',
    session_id: search.session_id ?? '',
    attempt_id: search.attempt_id ?? '',
    provider: search.provider ?? '',
    hours: search.hours ?? 24
  };
}

/** The URL for a draft. Empty choices are dropped so the URL only carries what is set. */
export function searchFrom(draft: HealthSearch): HealthSearch {
  return Object.fromEntries(
    Object.entries(draft).filter(([, value]) => value !== '' && value !== undefined)
  ) as HealthSearch;
}

export interface Choice {
  value: string;
  label: string;
}

function unique(choices: Choice[]): Choice[] {
  const seen = new Map<string, Choice>();
  for (const choice of choices) if (!seen.has(choice.value)) seen.set(choice.value, choice);
  return [...seen.values()];
}

/** A selected value that is not among the options stays selectable, under a plain fallback label. */
export function withSelected(options: Choice[], selected: string, fallback: string): Choice[] {
  if (!selected || options.some((option) => option.value === selected)) return options;
  return [...options, { value: selected, label: fallback }];
}

export function projectChoices(projects: Project[]): Choice[] {
  return unique(projects.map((project) => ({ value: project.id, label: project.name })));
}

export function taskChoices(rows: HealthRow[]): Choice[] {
  return unique(
    rows.flatMap((row) =>
      row.task_id ? [{ value: row.task_id, label: row.title ?? row.task_id }] : []
    )
  );
}

export function sessionChoices(rows: HealthRow[]): Choice[] {
  return unique(
    rows.flatMap((row) =>
      row.sessions.flatMap((session) =>
        session.session_id
          ? [{ value: session.session_id, label: session.title ?? session.session_id }]
          : []
      )
    )
  );
}

export function attemptChoices(rows: HealthRow[]): Choice[] {
  return unique(
    rows.flatMap((row) =>
      row.sessions.flatMap((session) =>
        session.attempt_id
          ? [
              {
                value: session.attempt_id,
                label: `${session.title ?? session.session_id ?? 'Session'} · ${session.attempt_id.slice(0, 8)}`
              }
            ]
          : []
      )
    )
  );
}

export const MALFORMED_VIEW = 'Mainloop sent health data this page cannot read.';
export const MALFORMED_OBSERVATIONS = 'Mainloop sent observations this page cannot read.';

/** The copy for a failed read. The page keeps its own title. */
export function healthFailureText(kind: LoadFailure): string {
  if (kind === 'server') return 'Mainloop returned a server error.';
  if (kind === 'unreachable') return "Can't reach the Mainloop backend.";
  return 'Try again in a moment.';
}
