import { SessionControls } from '@/components/shared/controls-sheet';
import { ComposerFooter } from './ComposerFooter';

const controls = (mode: string) => (
  <>
    <SessionControls
      label="Claude · sonnet-5-5"
      title="Session controls"
      open={false}
      onOpenChange={() => {}}
    >
      {null}
    </SessionControls>
    <button type="button" className="min-h-8 flex-none">
      ⏵⏵ {mode}
    </button>
  </>
);

const history = (
  <button
    type="button"
    aria-label="Sent messages"
    aria-expanded={false}
    className="px-xs min-h-8 flex-none"
  >
    ↑ history
  </button>
);

/**
 * The live composer's footer with its longest labels. On a phone the directory and the history
 * toggle wrap to a second line; the directory truncates and the toggle stays on screen.
 */
export default function ComposerFooterPreview() {
  return (
    <div className="bg-term-bg text-term-fg p-sm gap-md flex flex-col">
      <ComposerFooter
        controls={controls('bypass permissions')}
        cwd="/data/workspace"
        history={history}
      />
      <ComposerFooter
        controls={controls('default')}
        cwd="/data/workspace/example-project/services/api/internal/handlers"
        history={history}
      />
      <ComposerFooter controls={controls('accept edits')} cwd="/data/workspace" />
    </div>
  );
}
