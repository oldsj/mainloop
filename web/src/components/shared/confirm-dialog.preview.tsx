import { ConfirmDialog } from './confirm-dialog';

const noop = () => {};

/** The two confirm shapes: a destructive action and a plain one, both open, plus the pending state. */
export default function ConfirmDialogPreview() {
  return (
    <div className="gap-md p-md grid">
      <ConfirmDialog
        open
        onOpenChange={noop}
        title="Cancel this task?"
        description="The running attempt is stopped. This cannot be undone."
        confirmLabel="Cancel task"
        cancelLabel="Keep running"
        destructive
        onConfirm={noop}
      />
      <ConfirmDialog
        open
        onOpenChange={noop}
        title="Delete this schedule?"
        description="Future runs stop. Past executions stay in the list."
        confirmLabel="Delete"
        pending
        onConfirm={noop}
      />
    </div>
  );
}
