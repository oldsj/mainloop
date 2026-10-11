import type { HITLView } from '@/lib/contract';
import type { HITLState } from '@/lib/hitl';
import { decisionReceipt, hitlView } from '@/test/fixtures/hitl';
import { HITLRequest } from './hitl-request';

const noop = () => {};
const never = () => Promise.reject(new Error('Not available in the preview.'));

function snapshot(view: HITLView | null, patch: Partial<HITLState> = {}): HITLState {
  return { view, busy: false, error: null, uncertain: false, stale: false, ...patch };
}

const question = hitlView('question', {
  request: {
    ...hitlView('question').request,
    payload: {
      type: 'ask_user_request',
      id: 'q',
      nested: null,
      questions: [
        {
          question: 'Which systems should the tests cover?',
          choices: ['Linux', 'macOS'],
          multiple: true
        },
        { question: 'Anything else the agent should know?', choices: null, multiple: false }
      ]
    }
  }
});

const answered: HITLView = (() => {
  const base = hitlView('answered', { context: { provider: 'claude' } });
  return { ...base, response: decisionReceipt(base.request), transport_state: 'accepted' };
})();

/** The request card in the states an owner meets: waiting, questions, locked, off, recorded. */
export default function HITLRequestPreview() {
  const states: [string, HITLState][] = [
    [
      'Waiting for a tool decision',
      snapshot(hitlView('waiting', { context: { provider: 'codex' } }))
    ],
    ['Questions', snapshot(question)],
    ['Submitting', snapshot(hitlView('submitting'), { busy: true })],
    [
      'Delivery uncertain, locked',
      snapshot(hitlView('uncertain'), {
        uncertain: true,
        stale: true,
        error: 'Decision delivery uncertain. Checking the recorded result; do not submit again.'
      })
    ],
    ['Owner writes off', snapshot(hitlView('off', { writes_enabled: false }))],
    ['Recorded and delivered', snapshot(answered)],
    [
      'Agent payload not understood',
      snapshot({
        ...hitlView('bad'),
        request: { ...hitlView('bad').request, payload: { type: 'bad' } as never }
      })
    ],
    ['Loading', snapshot(null)]
  ];
  return (
    <div className="gap-md p-md grid">
      {states.map(([label, state]) => (
        <section key={label} className="gap-sm grid">
          <h2 className="text-muted-foreground text-sm">{label}</h2>
          <div className="border-border rounded-none border">
            <HITLRequest
              snapshot={state}
              onRespond={noop}
              onRefresh={noop}
              onLoadMergeDetails={never}
            />
          </div>
        </section>
      ))}
    </div>
  );
}
