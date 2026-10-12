import { messageFixture } from './fixtures';
import { MessageBubble, STOPPED_NOTE } from './MessageBubble';

/** An owner message, an assistant reply with markdown, a stopped turn, child reports, and a failed send. */
export default function MessageBubblePreview() {
  return (
    <div className="gap-sm flex flex-col">
      <MessageBubble message={messageFixture({ content: 'Fix the flaky test in `server/`.' })} />
      <MessageBubble
        agentKind="claude"
        message={messageFixture({
          id: 'a',
          role: 'assistant',
          content:
            'Done. See [the PR](https://example.com/pr/1).\n\n- one\n- two\n\n| a | b |\n| - | - |\n| 1 | 2 |'
        })}
      />
      <MessageBubble
        agentKind="claude"
        message={messageFixture({
          id: 'b',
          role: 'assistant',
          content: `Looking at the logs\n\n${STOPPED_NOTE}`
        })}
      />
      <MessageBubble
        message={messageFixture({
          id: 'c',
          content:
            '[report from child task 0a1b2c3d-1111-4222-8333-444455556666 attempt 1; completed; unverified claim]\nAll checks pass.'
        })}
      />
      <MessageBubble
        message={messageFixture({
          id: 'd',
          content:
            '[report from child task 9f8e7d6c-1111-4222-8333-444455556666 attempt 2; blocked; unverified claim]\nThe schema is ambiguous.\nOwner action needed: decide the schema'
        })}
      />
      <MessageBubble
        message={messageFixture({ id: 'e', content: 'Deploy it' })}
        notice={{
          state: 'failed',
          label: 'Not sent',
          reason: 'not sent: agent offline',
          retryable: true
        }}
        onRetry={() => {}}
      />
    </div>
  );
}
