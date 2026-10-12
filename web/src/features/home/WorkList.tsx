import { Link } from '@tanstack/react-router';
import { useEffect, useState } from 'react';
import { $api } from '@/api';
import type { components } from '@/api/schema.gen';
import type { TaskView } from '@/lib/contract';
import { useTaskList } from '@/features/tasks';
import { relativeTime, groupWork, type WorkGroup, type WorkRow } from './model';

type Project = components['schemas']['Project'];

const HEADINGS: Record<WorkGroup, string> = {
  'needs-you': 'Needs you',
  working: 'Working',
  blocked: 'Blocked or failed',
  finished: 'Finished since you last looked'
};

const GROUP_ORDER: WorkGroup[] = ['needs-you', 'working', 'blocked', 'finished'];

/**
 * Tasks grouped by what the owner should do about them. Running tasks are current work, so the
 * Working group is always shown; with nothing running it points to the main chat rather than
 * implying the owner has nothing to do. Other groups show only when they have rows.
 */
export function WorkList({ lastVisit }: { lastVisit: number | null }) {
  const now = useClock();
  const tasks = useTaskList();
  const projects = $api.useQuery('get', '/projects');
  const names = projectNames(projects.data);

  return (
    <section aria-labelledby="work-heading" data-testid="work-list" className="min-w-0">
      <h2 id="work-heading" className="px-md pt-md text-sm font-medium">
        Work
      </h2>
      {tasks.isError && tasks.data === undefined ? (
        <p role="alert" className="px-md py-sm text-term-red text-sm">
          Couldn’t load tasks. This list fills in when the next read succeeds.
        </p>
      ) : tasks.data === undefined ? (
        <p role="status" className="px-md py-sm text-muted-foreground text-sm">
          Checking your tasks…
        </p>
      ) : (
        <Groups tasks={tasks.data} lastVisit={lastVisit} names={names} now={now} />
      )}
    </section>
  );
}

function Groups({
  tasks,
  lastVisit,
  names,
  now
}: {
  tasks: TaskView[];
  lastVisit: number | null;
  names: Map<string, string>;
  now: number;
}) {
  const groups = groupWork(tasks, lastVisit);
  return (
    <>
      {GROUP_ORDER.map((group) => {
        const rows = groups[group];
        if (rows.length === 0 && group !== 'working') return null;
        return <GroupSection key={group} group={group} rows={rows} names={names} now={now} />;
      })}
    </>
  );
}

/** Rows shown per group before "Show all", so a long list cannot push Sessions and Inbox away. */
const ROW_CAP = 5;

function GroupSection({
  group,
  rows,
  names,
  now
}: {
  group: WorkGroup;
  rows: WorkRow[];
  names: Map<string, string>;
  now: number;
}) {
  const [expanded, setExpanded] = useState(false);
  const shown = expanded ? rows : rows.slice(0, ROW_CAP);
  return (
    <section aria-labelledby={`work-${group}`} className="px-md py-sm min-w-0">
      <h2 id={`work-${group}`} className="text-muted-foreground text-xs font-medium">
        {HEADINGS[group]} {rows.length > 0 && <span>({rows.length})</span>}
      </h2>
      {rows.length === 0 ? (
        <p className="text-muted-foreground mt-xs text-sm">
          Nothing running. Start work from the main chat.
        </p>
      ) : (
        <>
          <ul className="mt-xs min-w-0">
            {shown.map((row) => (
              <WorkRowItem
                key={row.view.task.id}
                row={row}
                project={names.get(row.view.task.project_id ?? '') ?? null}
                now={now}
              />
            ))}
          </ul>
          {rows.length > ROW_CAP ? (
            <button
              type="button"
              aria-expanded={expanded}
              onClick={() => setExpanded((value) => !value)}
              className="text-muted-foreground mt-xs min-h-11 text-xs underline"
            >
              {expanded ? 'Show fewer' : `Show all (${rows.length})`}
            </button>
          ) : null}
        </>
      )}
    </section>
  );
}

/** The current time, refreshed each minute so relative times age without a network read. */
function useClock(): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 60_000);
    return () => clearInterval(timer);
  }, []);
  return now;
}

function WorkRowItem({ row, project, now }: { row: WorkRow; project: string | null; now: number }) {
  const { task } = row.view;
  return (
    <li className="border-border min-w-0 border-b last:border-b-0">
      <Link
        to="/tasks/$id"
        params={{ id: task.id }}
        data-testid="work-row"
        className="gap-xs py-sm flex min-h-11 min-w-0 flex-col"
      >
        <span className="truncate text-sm font-medium">{task.title}</span>
        <span className="text-muted-foreground gap-x-sm flex min-w-0 flex-wrap text-xs">
          <span className="truncate">{project ?? 'No project'}</span>
          <span className="min-w-0">{row.statusLine}</span>
          <time dateTime={task.updated_at}>{relativeTime(task.updated_at, now)}</time>
        </span>
      </Link>
    </li>
  );
}

function projectNames(projects: Project[] | undefined): Map<string, string> {
  const names = new Map<string, string>();
  for (const project of projects ?? []) names.set(project.id, `${project.owner}/${project.name}`);
  return names;
}
