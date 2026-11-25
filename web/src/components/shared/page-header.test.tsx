import { render, screen, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { BACK_LINK, BackIcon, PageHeader } from './page-header';

describe('page header', () => {
  it('puts back, the title, the status and the actions on one row, in that order', () => {
    render(
      <PageHeader
        title="Schedule"
        back={
          <a href="/schedules" aria-label="Back to scheduled runs" className={BACK_LINK}>
            <BackIcon />
          </a>
        }
        status={<span data-testid="status">paused</span>}
        actions={<button type="button">Run now</button>}
        description="Runs at nine every weekday."
      />
    );
    const header = screen.getByTestId('page-header');
    const row = header.firstElementChild as HTMLElement;
    expect([...row.children].map((child) => child.textContent)).toEqual([
      '',
      'Schedule',
      'paused',
      'Run now'
    ]);
    // The back link is named by where it goes; its arrow is decoration.
    expect(within(row).getByRole('link', { name: 'Back to scheduled runs' })).toBeInTheDocument();
    expect(row.querySelector('svg')).toHaveAttribute('aria-hidden', 'true');
    // The description sits under the row.
    expect(header.lastElementChild).toHaveTextContent('Runs at nine every weekday.');
  });

  it('is the page’s one h1, at the same size on every page', () => {
    render(<PageHeader title="Settings" headingId="settings-heading" />);
    const title = screen.getByRole('heading', { level: 1, name: 'Settings' });
    expect(title).toHaveClass('text-display', 'font-semibold');
    expect(title).toHaveAttribute('id', 'settings-heading');
    // A top-level page has no back link, no actions and no description.
    expect(screen.queryByRole('link')).toBeNull();
    expect(screen.getByTestId('page-header').children).toHaveLength(1);
  });
});
