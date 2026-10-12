import { Link } from '@tanstack/react-router';
import type { TaskView } from '@/lib/contract';
import { countText, type SinceCounts } from './model';

interface Item {
  key: keyof SinceCounts;
  label: string;
  tone: string;
}

const ITEMS: Item[] = [
  { key: 'waiting', label: 'waiting on you', tone: 'text-term-yellow' },
  { key: 'failed', label: 'failed', tone: 'text-term-red' },
  { key: 'merged', label: 'merged', tone: 'text-term-green' }
];

/**
 * "Since you were here": counts of what changed since the last visit. Each count links to the most
 * recent task in that count. Renders nothing when nothing changed, so an empty strip never shows.
 * The compact form is one line that scrolls sideways rather than wrapping, for the phone chat tab.
 */
export function SinceStrip({
  counts,
  compact = false
}: {
  counts: SinceCounts;
  compact?: boolean;
}) {
  const shown = ITEMS.flatMap((item) => {
    const [latest] = counts[item.key];
    return latest ? [{ ...item, latest, count: counts[item.key].length }] : [];
  });
  if (shown.length === 0) return null;
  return (
    <section
      aria-label="Since you were here"
      data-testid="since-strip"
      className={
        compact
          ? 'gap-sm px-md py-xs border-border flex min-w-0 items-center border-b'
          : 'px-md py-sm'
      }
    >
      {compact ? (
        <p className="text-muted-foreground shrink-0 text-xs font-medium">Since you were here</p>
      ) : (
        <h2 className="text-muted-foreground text-xs font-medium">Since you were here</h2>
      )}
      <ul
        className={
          compact
            ? 'gap-sm flex min-w-0 flex-1 items-center overflow-x-auto whitespace-nowrap'
            : 'gap-sm mt-xs flex flex-wrap'
        }
      >
        {shown.map(({ key, label, tone, latest, count }) => (
          <li key={key} className="shrink-0">
            <SinceLink
              task={latest}
              text={`${countText(count, 'task')} ${label}`}
              tone={tone}
              testId={`since-${key}`}
            />
          </li>
        ))}
      </ul>
    </section>
  );
}

function SinceLink({
  task,
  text,
  tone,
  testId
}: {
  task: TaskView;
  text: string;
  tone: string;
  testId: string;
}) {
  return (
    <Link
      to="/tasks/$id"
      params={{ id: task.task.id }}
      data-testid={testId}
      className={`border-border px-sm py-xs inline-flex min-h-11 items-center border text-xs ${tone}`}
    >
      {text}
    </Link>
  );
}
