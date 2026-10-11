import { AlertDialog } from 'radix-ui';
import { useRef } from 'react';
import { Button } from '@/components/ui/button';

export interface ConfirmDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description: string;
  confirmLabel: string;
  cancelLabel?: string;
  /** Uses the destructive style on the confirm button. */
  destructive?: boolean;
  /** The action is in flight: both buttons are disabled and Escape or an overlay click does nothing. */
  pending?: boolean;
  onConfirm: () => void;
}

/**
 * Two-step confirmation for an action that cannot be undone or costs something (cancel a task,
 * delete a schedule). Built on the Radix alert dialog: it traps focus, is announced as an alert,
 * and focuses Cancel first so Enter does not confirm by accident.
 */
export function ConfirmDialog({
  open,
  onOpenChange,
  title,
  description,
  confirmLabel,
  cancelLabel = 'Cancel',
  destructive = false,
  pending = false,
  onConfirm
}: ConfirmDialogProps) {
  // Radix returns focus to its own Trigger, and this dialog is opened from state, so there is none
  // and focus would drop to <body>. Remember what had focus when the dialog opened and return to it.
  const opener = useRef<HTMLElement | null>(null);
  return (
    <AlertDialog.Root
      open={open}
      onOpenChange={(next) => {
        if (!pending) onOpenChange(next);
      }}
    >
      <AlertDialog.Portal>
        <AlertDialog.Overlay className="bg-background/80 fixed inset-0" />
        <AlertDialog.Content
          onOpenAutoFocus={() => {
            opener.current =
              document.activeElement instanceof HTMLElement ? document.activeElement : null;
          }}
          onCloseAutoFocus={(event) => {
            event.preventDefault();
            opener.current?.focus();
          }}
          className="inset-x-md gap-md border-border bg-background p-lg text-foreground fixed top-1/2 mx-auto grid max-w-md -translate-y-1/2 rounded-none border"
        >
          <AlertDialog.Title className="text-lg font-medium">{title}</AlertDialog.Title>
          <AlertDialog.Description className="text-muted-foreground text-sm">
            {description}
          </AlertDialog.Description>
          <div className="gap-sm flex flex-col-reverse sm:flex-row sm:justify-end">
            <AlertDialog.Cancel asChild>
              <Button variant="outline" disabled={pending}>
                {cancelLabel}
              </Button>
            </AlertDialog.Cancel>
            <Button
              variant={destructive ? 'destructive' : 'default'}
              disabled={pending}
              onClick={onConfirm}
            >
              {confirmLabel}
            </Button>
          </div>
        </AlertDialog.Content>
      </AlertDialog.Portal>
    </AlertDialog.Root>
  );
}
