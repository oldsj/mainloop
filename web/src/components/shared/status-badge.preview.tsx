import type { SessionStatus } from '@/lib/contract';
import { statusLabels } from '@/lib/sessionStatus';
import { StatusBadge } from './status-badge';
import { SESSION_STATUS_TONE } from './status-tokens';

/** Every session status in its token colour, plus the owner-attention variant. */
export default function StatusBadgePreview() {
  const statuses = Object.keys(statusLabels) as SessionStatus[];
  return (
    <ul className="gap-sm p-md flex flex-wrap">
      {statuses.map((status) => (
        <li key={status}>
          <StatusBadge label={statusLabels[status]} tone={SESSION_STATUS_TONE[status]} />
        </li>
      ))}
      <li>
        <StatusBadge label="NEEDS YOU" tone="attention" />
      </li>
    </ul>
  );
}
