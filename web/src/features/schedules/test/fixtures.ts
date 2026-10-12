import type { ScheduleExecution, ScheduleView } from '../model';

export function schedule(overrides: Partial<ScheduleView> = {}): ScheduleView {
  return {
    id: 'sch-1',
    agent: { namespace: 'agents', name: 'triage' },
    config: {
      name: 'Nightly triage',
      schedule: '0 2 * * *',
      time_zone: 'UTC',
      prompt: 'Triage the inbox',
      paused: false,
      execution_timeout_seconds: 900
    },
    etag: 'etag-1',
    next_execution_time: '2026-10-12T02:00:00Z',
    created_at: '2026-10-10T09:00:00Z',
    updated_at: '2026-10-10T09:00:00Z',
    ...overrides
  };
}

export function execution(overrides: Partial<ScheduleExecution> = {}): ScheduleExecution {
  return {
    id: 'exe-1',
    scheduled_run_id: 'sch-1',
    scheduled_time: '2026-10-11T02:00:00Z',
    manual_request_id: null,
    prompt: 'Triage the inbox',
    created_at: '2026-10-11T02:00:01Z',
    deadline: '2026-10-11T02:15:01Z',
    session_id: 'sess-1',
    state: 'succeeded',
    task_id: null,
    completed_at: '2026-10-11T02:04:00Z',
    failure_reason: null,
    ...overrides
  };
}
