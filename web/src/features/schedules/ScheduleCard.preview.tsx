import { ScheduleCard } from './ScheduleCard';

export default function ScheduleCardPreview() {
  return (
    <section aria-label="Active schedule" className="border-border p-md w-full border">
      <ScheduleCard
        name="Nightly triage"
        schedule="0 2 * * *"
        timeZone="Europe/London"
        agent="claude/triage-agent"
        paused={false}
        nextRun="2026-10-12T02:00:00Z"
      />
    </section>
  );
}

export function ScheduleCardPausedPreview() {
  return (
    <section aria-label="Paused schedule" className="border-border p-md w-full border">
      <ScheduleCard
        name="Weekly dependency sweep"
        schedule="30 9 * * 1"
        timeZone="UTC"
        agent="codex/sweep-agent"
        paused
        nextRun={null}
      />
    </section>
  );
}

export function ScheduleCardInvalidPreview() {
  return (
    <section aria-label="Schedule with problems" className="border-border p-md w-full border">
      <ScheduleCard
        name=""
        schedule="99 * * * *"
        timeZone="UTC"
        agent="claude/triage-agent"
        paused={false}
        nextRun={null}
      />
    </section>
  );
}
