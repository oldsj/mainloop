import { DotfilesRefreshView } from './DotfilesRefresh';
import type { DotfilesHint } from './model';

const changed: DotfilesHint = {
  text: 'dotfiles changed: a1b2c3d → e4f5a6b',
  compareUrl: 'https://github.com/example/dotfiles/compare/a1b2c3d...e4f5a6b',
  problem: null
};

const base = { busy: false, offline: false, done: null, error: null, onRefresh: () => {} };

/** Changed, refreshing, not confirmed, failed, refused by the server, and refreshed. */
export default function DotfilesRefreshPreview() {
  return (
    <div className="gap-md flex flex-col">
      <DotfilesRefreshView {...base} hint={changed} />
      <DotfilesRefreshView {...base} hint={changed} busy />
      <DotfilesRefreshView
        {...base}
        hint={{ ...changed, problem: { pending: true, text: 'refresh not confirmed yet' } }}
      />
      <DotfilesRefreshView
        {...base}
        hint={{
          ...changed,
          problem: {
            pending: false,
            text: 'last refresh failed: kagent cannot update a running session yet'
          }
        }}
      />
      <DotfilesRefreshView
        {...base}
        hint={changed}
        error="This session has no native session running; the next one is created with the current dotfiles."
      />
      <DotfilesRefreshView
        {...base}
        hint={null}
        done="dotfiles refreshed to e4f5a6b; the next turn reads them"
      />
    </div>
  );
}
