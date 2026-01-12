/**
 * Thread-aware types for Slack-like conversation model
 *
 * Mental model:
 * - Main thread = Channel (your continuous conversation)
 * - Async work = Threads (branch off from channel messages)
 */

export type ThreadStatus = 'active' | 'waiting' | 'completed' | 'failed';

// A message within a thread (worker ↔ user back-and-forth)
export interface ThreadMessage {
  id: string;
  role: 'worker' | 'user';
  content: string;
  timestamp: string;
}

// A thread is async work attached to a parent message
export interface Thread {
  id: string;
  parentMessageId: string;
  title: string;
  status: ThreadStatus;
  messages: ThreadMessage[];
  unreadCount: number;
  // Optional result data
  result?: {
    prUrl?: string;
    prNumber?: number;
    planText?: string;
  };
}

// A message in the main channel (can have an attached thread)
export interface ChannelMessage {
  id: string;
  role: 'user' | 'assistant';
  content: string;
  timestamp: string;
  thread?: Thread;
}

// Status config for thread display
// Note: Using complete class names for Tailwind detection (dynamic classes don't work)
export const threadStatusConfig: Record<
  ThreadStatus,
  { textClass: string; hoverBorderClass: string; icon: string; label: string }
> = {
  active: {
    textClass: 'text-term-info',
    hoverBorderClass: 'hover:border-term-info',
    icon: '⟳',
    label: 'Working'
  },
  waiting: {
    textClass: 'text-term-warning',
    hoverBorderClass: 'hover:border-term-warning',
    icon: '?',
    label: 'Waiting for you'
  },
  completed: {
    textClass: 'text-term-success',
    hoverBorderClass: 'hover:border-term-success',
    icon: '✓',
    label: 'Complete'
  },
  failed: {
    textClass: 'text-term-error',
    hoverBorderClass: 'hover:border-term-error',
    icon: '!',
    label: 'Failed'
  }
};

// Helper to get latest thread message
export function getLatestThreadMessage(thread: Thread): ThreadMessage | undefined {
  return thread.messages[thread.messages.length - 1];
}

// Helper to count replies
export function getReplyCount(thread: Thread): number {
  return thread.messages.length;
}
