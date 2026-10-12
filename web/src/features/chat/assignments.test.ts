import { describe, expect, it } from 'vitest';
import { assignmentIds } from './assignments';
import { messageFixture } from './fixtures';

describe('assignmentIds', () => {
  it('names the messages Mainloop sent and leaves the owner’s, replies and unmarked ones out', () => {
    const ids = assignmentIds([
      messageFixture({ id: 'brief', source: 'mainloop' }),
      messageFixture({ id: 'own', source: 'user' }),
      messageFixture({ id: 'optimistic' }),
      messageFixture({ id: 'reply', role: 'assistant' })
    ]);
    expect([...ids]).toEqual(['brief']);
  });
});
