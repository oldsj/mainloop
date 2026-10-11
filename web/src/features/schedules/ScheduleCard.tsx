import { StatusBadge } from '@/components/shared/status-badge';
import { describeCron, formatInZone } from './model';

export interface ScheduleCardProps {
  name: string;
  /** The five-field expression, shown under the plain-words line so the exact text is visible. */
  schedule: string;
  timeZone: string;
  /** `namespace/name` of the Agent. */
  agent: string;
  paused: boolean;
  nextRun: string | null;
}

/**
 * One schedule at a glance: its name, when it runs in words, its state and its next run. It holds
 * no data fetching, so the list and the design preview render it the same way. Wrap it in a link
 * where the whole card should be clickable.
 */
export function ScheduleCard({
  name,
  schedule,
  timeZone,
  agent,
  paused,
  nextRun
}: ScheduleCardProps) {
  const cron = describeCron(schedule);
  return (
    <div className="gap-sm flex min-w-0 flex-col">
      <div className="gap-sm flex flex-wrap items-center justify-between">
        <h2 className="text-foreground text-sm font-medium break-words">
          {name || 'Untitled schedule'}
        </h2>
        <StatusBadge label={paused ? 'Paused' : 'Active'} tone={paused ? 'muted' : 'ok'} />
      </div>
      <p className={`text-sm break-words ${cron.ok ? 'text-foreground' : 'text-destructive'}`}>
        {cron.ok ? cron.text : cron.message}
      </p>
      <p className="text-muted-foreground font-mono text-xs break-all">{schedule}</p>
      <dl className="gap-x-md gap-y-xs text-muted-foreground grid grid-cols-[auto_1fr] text-xs">
        <dt>Agent</dt>
        <dd className="text-foreground break-all">{agent}</dd>
        <dt>Time zone</dt>
        <dd className="text-foreground break-all">{timeZone}</dd>
        <dt>Next run</dt>
        <dd className="text-foreground">{paused ? 'Paused' : formatInZone(nextRun, timeZone)}</dd>
      </dl>
    </div>
  );
}
