import { Link } from '@tanstack/react-router';
import { $api } from '@/api';
import { statusLabel, taskForApproval } from '@/lib/taskState';
import { hitlClient, loadMergeDetails, useHITL } from './hitl-client';
import { HITLRequest } from './hitl-request';

export interface HITLCardProps {
  /** The approval or question request to show. The card reads its owning task from the cache. */
  requestId: string;
}

/**
 * One request from the inbox, a conversation or the health page. All mounts of one request id
 * share one state machine, so a submit in one place locks the others.
 */
export function HITLCard({ requestId }: HITLCardProps) {
  const snapshot = useHITL(requestId);
  const projectId = snapshot.view?.context?.project_id ?? null;
  const tasks = $api.useQuery(
    'get',
    '/tasks',
    { params: { query: { project_id: projectId } } },
    { enabled: !!projectId }
  );
  const task = tasks.data ? taskForApproval(tasks.data, requestId) : null;

  return (
    <div className="gap-sm grid">
      {task ? (
        <p data-testid="hitl-task-link" className="px-md text-sm">
          <Link
            to="/tasks/$id"
            params={{ id: task.task.id }}
            className="text-primary inline-flex min-h-11 items-center underline underline-offset-4"
          >
            {task.task.title}
          </Link>{' '}
          · {statusLabel(task.task.status)}
        </p>
      ) : null}
      <HITLRequest
        key={requestId}
        snapshot={snapshot}
        onRespond={(draft) => void hitlClient.respond(requestId, draft)}
        onRefresh={() => void hitlClient.refresh(requestId)}
        onLoadMergeDetails={(proposalId, section, cursor) =>
          loadMergeDetails(requestId, proposalId, section, cursor)
        }
      />
    </div>
  );
}
