import type { ReactNode } from 'react';

/**
 * The row under the live composer: the session controls and the permission mode, then the working
 * directory and the history toggle. The directory truncates first. Where even the controls, the
 * mode and the toggle do not fit on one line (a phone), the directory and the toggle wrap to a
 * second line together, so the toggle is never pushed off-screen.
 */
export function ComposerFooter({
  controls,
  cwd,
  history
}: {
  controls: ReactNode;
  cwd: string;
  history?: ReactNode;
}) {
  return (
    <div
      className="text-term-fg-muted gap-x-sm flex min-w-0 flex-wrap items-center text-xs"
      data-testid="composer-footer"
    >
      {controls}
      {/* Its smallest width is the toggle's: the directory has no width of its own and only fills
          what is left, so this group wraps as one rather than squeezing the toggle out. */}
      <span className="gap-sm flex flex-1 items-center">
        <span className="w-0 flex-1 truncate" title={cwd}>
          {cwd}
        </span>
        {history}
      </span>
    </div>
  );
}
