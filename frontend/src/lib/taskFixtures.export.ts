/** Offline schema-check input generated from the same factories the UI tests use. */
import { view, codexView, reassignedView, operation, projection } from './taskFixtures.ts';

console.log(
  JSON.stringify([
    view(),
    codexView(),
    reassignedView(),
    view({ mode: 'coordination', checkout: null }, { attempts: [] }),
    view(
      { status: 'blocked', reason: 'handoff' },
      {
        operations: [operation({ state: 'checkpoint_required', reason: 'handoff' })],
        projection: projection({ pending_approval_ids: ['approval-1'] })
      }
    ),
    view(
      { status: 'blocked', reason: 'reconciliation' },
      {
        operations: [
          operation({
            state: 'uncertain',
            last_confirmed_step: 'source_fencing',
            reason: 'reconciliation'
          })
        ],
        reports: [
          {
            task_id: 'task-1',
            attempt_id: 'attempt-1',
            request_id: 'report-1',
            summary: 'Fixture claim',
            outcome: 'progress'
          }
        ]
      }
    )
  ])
);
