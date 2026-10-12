import { useState } from 'react';
import { useNavigate, Link } from '@tanstack/react-router';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { agentOptions, createRequestIds, emptyDraft, messageOf, statusOf } from './model';
import { ScheduleForm } from './ScheduleForm';
import { useCreateSchedule, useProviders } from './queries';
import type { ScheduleConfigInput, ScheduleDraft } from './model';

/**
 * Creates a schedule. The request id is reused when the same body is sent again (a retry after a
 * dropped reply), so the server returns the schedule that already exists instead of making a
 * second one. A changed body gets a new id.
 */
export function ScheduleCreatePage() {
  const navigate = useNavigate();
  const providers = useProviders();
  const create = useCreateSchedule();
  const [requestIds] = useState(() => createRequestIds());
  const [initial] = useState(() => emptyDraft());
  const agents = agentOptions(providers.data);

  function submit(config: ScheduleConfigInput, draft: ScheduleDraft) {
    const option = agents.find((agent) => agent.value === draft.agent);
    if (!option) return;
    const body = { agent: option.ref, config };
    const request_id = requestIds.idFor(JSON.stringify(body));
    create.mutate(
      { body: { request_id, ...body } },
      {
        onSuccess: (created) => {
          requestIds.reset();
          navigate({ to: '/schedules/$id', params: { id: created.id } });
        },
        onError: (error) => {
          // 409 means the id was reused for a different body or the agent is gone, so the next try needs a fresh id.
          if (statusOf(error) === 409) requestIds.reset();
        }
      }
    );
  }

  const failed = create.error;

  return (
    <div className="gap-lg p-md flex min-w-0 flex-col">
      <header className="gap-sm flex flex-col">
        <p className="text-muted-foreground text-sm">
          <Link to="/schedules" className="text-foreground underline">
            Scheduled runs
          </Link>
        </p>
        <h1 className="text-xl font-medium">New schedule</h1>
      </header>

      {providers.isError ? (
        <ErrorRecovery
          title="Could not load agents"
          message={messageOf(providers.error)}
          onRetry={() => providers.refetch()}
          pending={providers.isFetching}
        />
      ) : null}

      <ScheduleForm
        mode="create"
        initial={initial}
        agents={agents}
        agentsLoading={providers.isPending}
        busy={create.isPending}
        error={failed ? `Not created. ${messageOf(failed)}` : null}
        onSubmit={submit}
        onCancel={() => navigate({ to: '/schedules' })}
      />
    </div>
  );
}
