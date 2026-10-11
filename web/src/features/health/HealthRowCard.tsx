import { Link } from '@tanstack/react-router';
import { useId, useState } from 'react';
import { HITLCard } from '@/features/hitl';
import { SessionObservations } from './SessionObservations';
import { HealthNativeStrip } from './HealthNativeStrip';
import {
  approvalText,
  coverageText,
  countText,
  gapText,
  type HealthRow,
  reportText,
  rowTitle,
  sessionLine,
  unreadableClaimsText,
  usageLine
} from './model';

/**
 * One task or session group. The summary is a disclosure button, so the whole row is one target.
 * Expanded, it shows the task link, collection gaps, blocked or failed claims, approvals and the
 * sessions. `generation` changes on each refresh, which clears any source observations shown.
 */
export function HealthRowCard({
  row,
  hours,
  generation
}: {
  row: HealthRow;
  hours: number;
  generation: number;
}) {
  const [expanded, setExpanded] = useState(false);
  const panelId = useId();
  const { totals } = row;

  return (
    <div>
      <button
        type="button"
        aria-expanded={expanded}
        aria-controls={panelId}
        onClick={() => setExpanded((value) => !value)}
        className="block min-h-11 w-full text-left"
      >
        <span className="gap-sm flex flex-wrap items-center justify-between">
          <span className="font-medium break-words">{rowTitle(row)}</span>
          <span className="text-muted-foreground text-xs">
            {coverageText(totals.coverage)} · Details
          </span>
        </span>
        <span className="gap-x-md gap-y-sm mt-sm grid grid-cols-2 text-sm sm:grid-cols-4">
          <Metric label="Failures" value={countText(totals.failures)} />
          <Metric label="Repeated groups" value={countText(totals.repeated_failure_groups)} />
          <Metric label="Input" value={countText(totals.input_tokens)} />
          <Metric label="Output" value={countText(totals.output_tokens)} />
        </span>
        <span className="text-muted-foreground mt-sm block text-xs">{usageLine(row)}</span>
      </button>

      {expanded ? (
        <div id={panelId} className="gap-md mt-md grid text-sm">
          {row.task_id ? (
            <Link
              to="/tasks/$id"
              params={{ id: row.task_id }}
              className="text-foreground inline-flex min-h-11 items-center underline underline-offset-4"
            >
              Open task
            </Link>
          ) : null}
          {row.gaps.length > 0 ? <p className="text-term-yellow">{gapText(row.gaps)}</p> : null}
          {row.unreadableClaims > 0 ? (
            <p className="text-term-yellow">{unreadableClaimsText(row.unreadableClaims)}</p>
          ) : null}
          {row.reports.map((report) => (
            <p key={report.id} className="text-muted-foreground">
              {reportText(report)}
            </p>
          ))}
          {row.approvals.map((approval) => (
            <div key={approval.id}>
              <p className="text-muted-foreground mb-sm">{approvalText(approval)}</p>
              <HITLCard requestId={approval.id} />
            </div>
          ))}
          {row.sessions.map((session) =>
            session.session_id ? (
              <section
                key={session.session_id}
                aria-label={`Session ${session.title ?? session.session_id}`}
                className="border-border pt-md gap-sm grid border-t"
              >
                <Link
                  to="/sessions/$id"
                  params={{ id: session.session_id }}
                  className="text-foreground inline-flex min-h-11 items-center break-words underline underline-offset-4"
                >
                  {session.title ?? session.session_id}
                </Link>
                <p className="text-muted-foreground">{sessionLine(session)}</p>
                <HealthNativeStrip sessionId={session.session_id} />
                <SessionObservations
                  key={generation}
                  sessionId={session.session_id}
                  hours={hours}
                />
              </section>
            ) : null
          )}
        </div>
      ) : null}
    </div>
  );
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <span>
      {label} <strong className="tabular-nums">{value}</strong>
    </span>
  );
}
