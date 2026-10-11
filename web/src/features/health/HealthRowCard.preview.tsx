import { HealthRowCard } from './HealthRowCard';
import type { HealthRow } from './model';
import { healthRow, totals } from './test/fixtures';

// The collapsed card only: its details open on a click, which a static preview does not make.
const measured = healthRow({
  title: 'Add retry button',
  totals: totals({ failures: 3, repeated_failure_groups: 1 })
}) as unknown as HealthRow;

const unmeasured = healthRow({
  id: 'row-2',
  task_id: null,
  title: null,
  sessions: [],
  totals: totals({ failures: null, repeated_failure_groups: null, coverage: 'partial' })
}) as unknown as HealthRow;

/** A row with known counts, and one with no task whose counts are not known. */
export default function HealthRowCardPreview() {
  return (
    <ul className="p-md">
      <li>
        <HealthRowCard row={measured} hours={24} generation={0} />
      </li>
      <li>
        <HealthRowCard row={unmeasured} hours={24} generation={0} />
      </li>
    </ul>
  );
}
