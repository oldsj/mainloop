import { useQueryClient } from '@tanstack/react-query';
import { useState, type FormEvent } from 'react';
import { TaskApiError } from '@/api';
import { Button } from '@/components/ui/button';
import type { TaskFollowUpResult, TaskView } from '@/lib/contract';
import { newRequestId } from '@/lib/taskActions';
import { rereadTask, sendFollowUp } from './queries';

/** The server's bound on a follow-up, in characters. */
export const MAX_FOLLOW_UP = 8000;

/** What each refusal means for the owner, in words. A reason not listed shows the server's code. */
const REFUSALS: Record<string, string> = {
  session_gone:
    "This task's native session is gone, so its context is lost. Start a new task to continue.",
  session_archived: "This task's session was archived. Start a new task to continue.",
  task_settling: 'The task is still finishing. Try again in a moment.',
  operation_pending: 'Another action on this task is in progress. Try again when it settles.',
  follow_up_code_task_completed:
    "This code task's pull request has merged. Start a new task for more work.",
  task_terminal: 'This task has ended and cannot take a follow-up.',
  task_not_running: 'The task is not running yet. Try again once it starts.',
  parent_not_active: "The task's parent has ended, so it cannot be reopened.",
  parent_capacity: 'The parent task is at its limit of open children.',
  stale_task_version: 'The task changed. Check it, then send again.'
};

type Notice = { tone: 'info' | 'error'; text: string; reason: string | null };

function sentNotice(result: TaskFollowUpResult): Notice {
  const how =
    result.delivery === 'queued' ? 'Queued behind the turn that is running.' : 'Sent to the task.';
  const reopened = result.reopened ? ' The task reopened on its session.' : '';
  return { tone: 'info', text: how + reopened, reason: null };
}

function failureNotice(error: unknown): Notice {
  if (error instanceof TaskApiError) {
    const reason = error.reason;
    return {
      tone: 'error',
      text: (reason && REFUSALS[reason]) ?? error.message,
      reason
    };
  }
  // No answer: the server may have recorded the message. Sending again reuses the request ID, so
  // the server answers the first send rather than sending twice.
  return {
    tone: 'error',
    text: 'Mainloop did not confirm the message. Send again to check; it will not be sent twice.',
    reason: null
  };
}

/**
 * A message to the task's own native session, so the agent keeps its context. The server decides
 * whether the task takes one (`view.actions.follow_up`): a running, waiting or blocked task takes it
 * as its next message, and a completed coordination task reopens on the same session.
 */
export function FollowUp({ view, offline }: { view: TaskView; offline: boolean }) {
  const queryClient = useQueryClient();
  const taskId = view.task.id;
  const [text, setText] = useState('');
  // Kept while the text is unchanged and the server has not answered, so a resend is the same
  // request. Any edit starts a new one.
  const [requestId, setRequestId] = useState<string | null>(null);
  const [sending, setSending] = useState(false);
  const [notice, setNotice] = useState<Notice | null>(null);

  if (!view.actions.follow_up?.available) return null;

  const trimmed = text.trim();
  const over = text.length > MAX_FOLLOW_UP;
  const enabled = !offline && !sending && trimmed !== '' && !over;

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!enabled) return;
    const id = requestId ?? newRequestId();
    setRequestId(id);
    setSending(true);
    setNotice(null);
    try {
      const result = await sendFollowUp(taskId, { request_id: id, text });
      setText('');
      setRequestId(null);
      setNotice(sentNotice(result));
      await rereadTask(queryClient, taskId);
    } catch (error) {
      // A refusal recorded nothing: the next send is a new request.
      if (error instanceof TaskApiError) setRequestId(null);
      setNotice(failureNotice(error));
    } finally {
      setSending(false);
    }
  }

  return (
    <section
      aria-labelledby="follow-up-heading"
      data-testid="task-follow-up"
      className="gap-sm p-md border-border-subtle flex flex-col border-t"
    >
      <form className="gap-sm flex flex-col" onSubmit={(event) => void submit(event)}>
        <label className="gap-xs flex flex-col">
          <span id="follow-up-heading" className="text-sm font-medium">
            Follow up
          </span>
          <textarea
            aria-describedby="follow-up-help"
            className="border-input bg-background text-foreground p-sm w-full rounded-none border text-sm"
            rows={3}
            value={text}
            disabled={sending}
            placeholder="Ask a question or give the next step"
            onChange={(event) => {
              setText(event.target.value);
              setRequestId(null);
            }}
          />
        </label>
        <p
          id="follow-up-help"
          className={over ? 'text-destructive text-xs' : 'text-muted-foreground text-xs'}
        >
          {over
            ? `${text.length.toLocaleString()} of ${MAX_FOLLOW_UP.toLocaleString()} characters.`
            : 'Goes to the agent on this task, which keeps what it already knows.'}
        </p>
        <div className="gap-sm flex flex-wrap items-center">
          <Button type="submit" data-testid="task-follow-up-send" disabled={!enabled}>
            {sending ? 'Sending…' : 'Send'}
          </Button>
          {offline ? (
            <span className="text-muted-foreground text-xs">Mainloop is offline.</span>
          ) : null}
        </div>
      </form>
      {notice ? (
        <div
          role={notice.tone === 'info' ? 'status' : 'alert'}
          data-testid="task-follow-up-notice"
          className={`gap-xs flex flex-col text-sm ${notice.tone === 'info' ? 'text-term-green' : 'text-destructive'}`}
        >
          <p className="break-words">{notice.text}</p>
          {notice.reason ? (
            <p className="text-xs">
              Server reason: <code>{notice.reason}</code>
            </p>
          ) : null}
        </div>
      ) : null}
    </section>
  );
}
