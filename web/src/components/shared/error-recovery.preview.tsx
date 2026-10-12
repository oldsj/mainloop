import { ErrorRecovery } from './error-recovery';

const noop = () => {};

/** The error block with and without a retry, and while a retry is in flight. */
export default function ErrorRecoveryPreview() {
  return (
    <div className="gap-md p-md grid">
      <ErrorRecovery
        title="Could not load the task"
        message="The server did not answer within 10 seconds. Nothing was changed."
        onRetry={noop}
      />
      <ErrorRecovery
        title="Reassign was refused"
        message="The task changed while you were looking at it. Reload to see the current version."
      />
      <ErrorRecovery
        title="Could not load the task"
        message="The server did not answer within 10 seconds."
        onRetry={noop}
        pending
      />
    </div>
  );
}
