import { ArrowLeft } from 'lucide-react';
import type { ReactNode } from 'react';
import { buttonVariants } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';

/**
 * The class for a page's back link: the icon button every page uses, so "back" is in the same place
 * and looks the same everywhere. The link itself is the caller's, so its route stays type-checked:
 *
 *     <Link to="/schedules" aria-label="Back to scheduled runs" className={BACK_LINK}>
 *       <BackIcon />
 *     </Link>
 */
export const BACK_LINK = cn(buttonVariants({ variant: 'ghost', size: 'icon' }), 'shrink-0');

/** The arrow inside a back link. Decoration: the link's `aria-label` says where it goes. */
export function BackIcon() {
  return <ArrowLeft aria-hidden="true" />;
}

export interface PageHeaderProps {
  /** The page's name: its one `h1`. */
  title: ReactNode;
  /** The way back, a link with the `BACK_LINK` class. A top-level page has none. */
  back?: ReactNode;
  /** A status beside the title, such as a badge. */
  status?: ReactNode;
  /** One or two muted sentences under the title. */
  description?: ReactNode;
  /** The page's own actions, on the right of the title row. */
  actions?: ReactNode;
  /** The `h1`'s id, when the page labels its region by it. */
  headingId?: string;
}

/**
 * The top of a page: back, title, status and actions on one row, with a description under it. One
 * component, so every page's title is the same size and its back control is in the same place.
 */
export function PageHeader({
  title,
  back,
  status,
  description,
  actions,
  headingId
}: PageHeaderProps) {
  return (
    <header className="gap-xs flex flex-col" data-testid="page-header">
      <div className="gap-sm flex min-w-0 flex-wrap items-center">
        {back}
        <h1 id={headingId} className="text-display min-w-0 flex-1 font-semibold break-words">
          {title}
        </h1>
        {status}
        {actions ? (
          <div className="gap-sm flex shrink-0 flex-wrap items-center">{actions}</div>
        ) : null}
      </div>
      {description ? (
        <p className="text-muted-foreground text-body max-w-prose">{description}</p>
      ) : null}
    </header>
  );
}
