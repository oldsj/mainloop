import { useEffect, useRef, type ReactNode } from 'react';
import { cn } from '@/components/ui/cn';

export interface SessionControlsProps {
  /** What the chip says: the provider, model and effort in use, such as "Claude · opus-5-5 · high". */
  label: string;
  /** The picker's accessible name. */
  title: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** The picker's sections. */
  children: ReactNode;
  className?: string;
  'data-testid'?: string;
}

/**
 * The chip that says how a session runs, and the one picker it opens. On a phone the picker is a
 * sheet at the bottom of the screen; on a wide screen it opens just above the chip.
 */
export function SessionControls({
  label,
  title,
  open,
  onOpenChange,
  children,
  className,
  'data-testid': testId
}: SessionControlsProps) {
  return (
    <span className="flex-none md:relative">
      <button
        type="button"
        className={cn(
          'hover:text-foreground min-h-control inline-flex items-center gap-1',
          open && 'text-foreground',
          className
        )}
        aria-haspopup="dialog"
        aria-expanded={open}
        onClick={() => onOpenChange(!open)}
        data-testid={testId}
      >
        {label}
        <span aria-hidden="true">▾</span>
      </button>
      {open ? (
        <ControlsSheet title={title} onClose={() => onOpenChange(false)}>
          {children}
        </ControlsSheet>
      ) : null}
    </span>
  );
}

/** The picker itself. Escape or a press outside closes it, and focus returns to where it was. */
function ControlsSheet({
  title,
  onClose,
  children
}: {
  title: string;
  onClose: () => void;
  children: ReactNode;
}) {
  const panel = useRef<HTMLDivElement>(null);
  const close = useRef(onClose);
  useEffect(() => {
    close.current = onClose;
  });
  useEffect(() => {
    const before = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    panel.current?.focus();
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') close.current();
    };
    document.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('keydown', onKey);
      before?.focus();
    };
  }, []);
  return (
    <>
      <button
        type="button"
        aria-label="Close"
        tabIndex={-1}
        onClick={onClose}
        className="bg-background/80 fixed inset-0 z-20 cursor-default md:bg-transparent"
      />
      <div
        ref={panel}
        role="dialog"
        aria-label={title}
        tabIndex={-1}
        className="bg-popover text-popover-foreground border-border p-md md:mb-xs md:pb-md fixed inset-x-0 bottom-0 z-30 flex max-h-[80%] flex-col gap-3 overflow-y-auto border-t pb-[calc(1rem+env(safe-area-inset-bottom,0px))] font-sans text-sm shadow-md outline-none md:absolute md:inset-x-auto md:bottom-full md:left-0 md:max-h-[70vh] md:w-[420px] md:border"
      >
        {children}
      </div>
    </>
  );
}

/** A section heading inside the picker. */
export function ControlsHeading({ children }: { children: ReactNode }) {
  return (
    <h3 className="text-muted-foreground text-2xs m-0 font-medium tracking-widest uppercase">
      {children}
    </h3>
  );
}
