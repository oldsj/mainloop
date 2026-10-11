import { ConfirmDialog } from './confirm-dialog';

const noop = () => {};

/** Open and destructive: the confirm uses the destructive token and Cancel has focus. */
export default function ConfirmDialogPreview() {
  return (
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
  );
}

/** Open and plain: the confirm uses the primary token. */
export function ConfirmDialogPlainPreview() {
  return (
    <ConfirmDialog
      open
      onOpenChange={noop}
      title="Retry this task?"
      description="A new attempt starts from the last checkpoint."
      confirmLabel="Retry task"
      onConfirm={noop}
    />
  );
}

/** Pending: both buttons are disabled and the dialog cannot be dismissed while the action runs. */
export function ConfirmDialogPendingPreview() {
  return (
    <ConfirmDialog
      open
      onOpenChange={noop}
      title="Delete this schedule?"
      description="Future runs stop. Past executions stay in the list."
      confirmLabel="Delete"
      destructive
      pending
      onConfirm={noop}
    />
  );
}
