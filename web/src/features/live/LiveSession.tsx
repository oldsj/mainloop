import { memo, useEffect, useLayoutEffect, useRef, useState, type KeyboardEvent } from 'react';
import { Markdown } from '@/components/markdown/markdown';
import { cn } from '@/components/ui/cn';
import {
  EFFORTS,
  MODE_IDS,
  MODE_LABELS,
  type Command,
  type Entry,
  type LiveState,
  type Perm
} from './store';
import { useLiveSession, type Conn } from './useLiveSession';

const short = (id: string) => (id || '…').replace(/^claude-/, '');

/**
 * A native Claude session driven live through its SDK runner: the transcript as Claude Code prints
 * it, permission prompts, interrupt, model and effort, permission mode, and a composer whose
 * messages queue while a turn runs. Phone first: one column that fills the page.
 */
export function LiveSession({
  sessionId,
  onFallback
}: {
  sessionId: string;
  /** Leaves the live view for the regular one, offered when the live channel is unavailable. */
  onFallback?: () => void;
}) {
  const live = useLiveSession(sessionId);
  return <LiveSessionView {...live} onFallback={onFallback} />;
}

export interface LiveSessionViewProps {
  state: LiveState;
  conn: Conn;
  send: (cmd: Record<string, unknown>) => boolean;
  answer: (permId: string, choice: number) => void;
  retry: () => void;
  onFallback?: () => void;
}

const CONN_LABEL: Record<Conn, string> = {
  attached: 'live',
  connecting: 'connecting…',
  reconnecting: 'reconnecting…',
  unavailable: 'unavailable'
};

export function LiveSessionView({
  state,
  conn,
  send,
  answer,
  retry,
  onFallback
}: LiveSessionViewProps) {
  const [sheet, setSheet] = useState(false);
  const busy = state.status !== 'idle';
  const interrupt = () => send({ k: 'interrupt' });
  const cycleMode = () => {
    const i = MODE_IDS.indexOf(state.mode as (typeof MODE_IDS)[number]);
    send({ k: 'mode', mode: MODE_IDS[(i + 1) % MODE_IDS.length] });
  };

  return (
    <section
      aria-label="Live session"
      data-testid="live-session"
      className="bg-term-bg text-term-fg flex min-h-0 flex-1 flex-col font-mono text-sm"
    >
      <div className="gap-sm px-md py-xs border-border flex items-center border-b text-xs">
        <span className={cn(statusClass(state.status))} data-testid="live-status">
          {statusLabel(state)}
        </span>
        <span className="flex-1" />
        <span
          data-testid="live-conn"
          className={conn === 'attached' ? 'text-term-green' : 'text-term-warning'}
        >
          {CONN_LABEL[conn]}
        </span>
        <button
          type="button"
          className="text-term-fg-muted hover:text-term-fg px-xs min-h-8"
          aria-label="Model"
          onClick={() => setSheet(true)}
        >
          {short(state.model)}
          {state.effort ? ` · ${state.effort}` : ''}
        </button>
      </div>

      {conn === 'unavailable' ? (
        <div
          role="alert"
          data-testid="live-unavailable"
          className="px-md py-sm border-term-warning mx-md mt-sm flex flex-col gap-2 border"
        >
          <div className="text-term-warning font-bold">Live view unavailable</div>
          <div className="text-term-fg-muted text-xs">
            The session&apos;s runner is not answering. It may be waking, or its agent may not run
            the SDK runner yet. Still retrying every few seconds.
          </div>
          <div className="flex flex-wrap gap-2">
            <button type="button" onClick={retry} className="border-border px-sm min-h-10 border">
              Retry now
            </button>
            {onFallback ? (
              <button
                type="button"
                onClick={onFallback}
                className="border-border px-sm min-h-10 border"
              >
                Use the chat view
              </button>
            ) : null}
          </div>
        </div>
      ) : null}

      <Log state={state} answer={answer} />

      <Composer
        state={state}
        busy={busy}
        send={send}
        interrupt={interrupt}
        cycleMode={cycleMode}
        openModel={() => setSheet(true)}
      />

      {sheet ? <ModelSheet state={state} send={send} close={() => setSheet(false)} /> : null}
    </section>
  );
}

function statusLabel(s: LiveState): string {
  if (s.runner === 'starting') return 'starting';
  if (s.status === 'waiting') return 'needs you';
  if (s.status === 'running') return 'working';
  return 'idle';
}

function statusClass(st: string) {
  return st === 'waiting'
    ? 'text-term-warning'
    : st === 'running'
      ? 'text-term-accent'
      : 'text-term-fg-muted';
}

function Log({ state, answer }: { state: LiveState; answer: LiveSessionViewProps['answer'] }) {
  const ref = useRef<HTMLDivElement>(null);
  const stick = useRef(true);
  useLayoutEffect(() => {
    const el = ref.current;
    if (el && stick.current) el.scrollTop = el.scrollHeight;
  });
  return (
    <div
      ref={ref}
      data-testid="live-log"
      aria-live="polite"
      onScroll={(e) => {
        const el = e.currentTarget;
        stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 60;
      }}
      className="gap-sm px-md py-sm flex min-h-0 flex-1 flex-col overflow-y-auto overscroll-contain"
    >
      {state.entries.map((e) => (
        <Row
          key={e.id}
          entry={e}
          perm={e.t === 'perm' ? state.perms[e.permId] : undefined}
          answer={answer}
        />
      ))}
      {state.activity ? (
        <div className="text-term-warning">
          ✻ {state.activity}… <span className="text-term-fg-muted text-xs">esc to interrupt</span>
        </div>
      ) : null}
    </div>
  );
}

const Row = memo(function Row({
  entry: e,
  perm,
  answer
}: {
  entry: Entry;
  perm?: Perm;
  answer: LiveSessionViewProps['answer'];
}) {
  switch (e.t) {
    case 'user':
      return (
        <div
          data-testid="live-user"
          className={cn(
            'bg-term-bg-secondary px-sm py-xs [overflow-wrap:anywhere] whitespace-pre-wrap',
            e.queued && 'opacity-60'
          )}
        >
          <span className="text-term-accent">❯ </span>
          {e.text}
          {e.queued ? (
            <span className="text-term-fg-muted text-xs"> queued · sends when this turn ends</span>
          ) : null}
        </div>
      );
    case 'text':
      return (
        <div data-testid="live-text" className="flex min-w-0">
          <span className="w-[2ch] flex-none" aria-hidden="true">
            ●
          </span>
          <Markdown className="flex-1">{e.text}</Markdown>
        </div>
      );
    case 'sys':
      return <div className="text-term-fg-muted pl-[2ch] text-xs">{e.text}</div>;
    case 'interrupted':
      return <div className="text-term-fg-muted pl-[2ch] text-xs">⎿ Interrupted</div>;
    case 'tool':
      return <ToolRow entry={e} />;
    case 'diff':
      return (
        <div className="border-border overflow-x-auto border text-xs">
          <div className="text-term-fg-muted px-sm py-xs border-border border-b">
            {e.verb} {e.file} · +{e.lines.filter((l) => l[0] === '+').length} −
            {e.lines.filter((l) => l[0] === '-').length}
          </div>
          <pre className="py-xs m-0 font-mono">
            {e.lines.map((l, i) => (
              <span
                key={i}
                className={cn('px-sm block', l[0] === '+' ? 'text-term-green' : 'text-term-error')}
              >
                {l[0]} {l[1]}
              </span>
            ))}
          </pre>
        </div>
      );
    case 'perm':
      return perm ? <PermCard perm={perm} answer={answer} /> : null;
  }
});

function ToolRow({ entry: e }: { entry: Extract<Entry, { t: 'tool' }> }) {
  const [open, setOpen] = useState(false);
  const hasOut = e.out.length > 0 && e.status !== 'run';
  return (
    <button
      type="button"
      data-testid="live-tool"
      aria-expanded={open}
      onClick={() => setOpen(!open)}
      className="block w-full min-w-0 text-left"
    >
      <span className="flex min-w-0 gap-[1ch]">
        <span
          aria-hidden="true"
          className={cn(
            'flex-none',
            e.status === 'run' && 'text-term-accent animate-pulse',
            e.status === 'ok' && 'text-term-green',
            e.status === 'err' && 'text-term-error'
          )}
        >
          ●
        </span>
        <span className="flex-none font-bold">{e.name}</span>
        <span
          className={cn(
            'text-term-fg-muted min-w-0',
            open ? '[overflow-wrap:anywhere] whitespace-pre-wrap' : 'truncate'
          )}
        >
          {e.arg}
        </span>
      </span>
      {hasOut ? (
        <span
          className={cn(
            'text-term-fg-muted block pl-[3ch] text-xs [overflow-wrap:anywhere] whitespace-pre-wrap',
            e.status === 'err' && 'text-term-error'
          )}
        >
          <span className="-ml-[2ch]">⎿ </span>
          {open
            ? e.out.join('\n')
            : e.out[0] + (e.out.length > 1 ? `  … +${e.out.length - 1} lines` : '')}
        </span>
      ) : null}
    </button>
  );
}

function PermCard({ perm, answer }: { perm: Perm; answer: LiveSessionViewProps['answer'] }) {
  const opts = ['Yes', perm.canAlways ? "Yes, and don't ask again this session" : null, 'No'];
  if (perm.chosen != null) {
    const label =
      perm.chosen === -1 ? 'cancelled' : perm.chosen === 2 ? 'No' : (opts[perm.chosen] ?? 'Yes');
    return (
      <div className="text-term-fg-muted text-xs [overflow-wrap:anywhere]">
        {perm.tool}({perm.arg.length > 48 ? perm.arg.slice(0, 48) + '…' : perm.arg}) →{' '}
        <span
          className={
            perm.chosen === 2 || perm.chosen === -1 ? 'text-term-error' : 'text-term-green'
          }
        >
          {label}
        </span>
      </div>
    );
  }
  return (
    <div
      role="group"
      aria-label={perm.title}
      data-testid="live-perm"
      className="border-term-warning px-sm py-xs flex flex-col gap-1 border"
    >
      <div className="text-term-warning font-bold">{perm.title}</div>
      <div className="pl-[2ch] [overflow-wrap:anywhere] whitespace-pre-wrap">{perm.arg}</div>
      {perm.why ? <div className="text-term-fg-muted pl-[2ch] text-xs">{perm.why}</div> : null}
      <div className="mt-1">Do you want to proceed?</div>
      {opts.map((o, k) =>
        o ? (
          <button
            key={k}
            type="button"
            onClick={() => answer(perm.id, k)}
            className="hover:bg-term-bg-secondary min-h-10 pl-[2ch] text-left"
          >
            {k === 2 && !perm.canAlways ? 2 : k + 1}. {o}
          </button>
        ) : null
      )}
    </div>
  );
}

function Composer({
  state,
  busy,
  send,
  interrupt,
  cycleMode,
  openModel
}: {
  state: LiveState;
  busy: boolean;
  send: LiveSessionViewProps['send'];
  interrupt: () => void;
  cycleMode: () => void;
  openModel: () => void;
}) {
  const [text, setText] = useState('');
  const [sel, setSel] = useState(0);
  const ta = useRef<HTMLTextAreaElement>(null);
  const matches: Command[] = /^\/\S*$/.test(text)
    ? [
        ...state.commands.filter((c) => c.name.startsWith(text)),
        ...state.commands.filter((c) => !c.name.startsWith(text) && c.name.includes(text.slice(1)))
      ]
    : [];
  const picked = matches[Math.min(sel, matches.length - 1)];
  const open = picked !== undefined && !(matches.length === 1 && picked.name === text);

  useEffect(() => {
    const el = ta.current;
    if (!el) return;
    el.style.height = 'auto';
    el.style.height = Math.min(el.scrollHeight, 150) + 'px';
  }, [text]);

  const submit = (value = text) => {
    const t = value.trim();
    if (!t) return;
    setText('');
    setSel(0);
    if (t === '/model') {
      openModel();
      return;
    }
    send({ k: 'user', text: t });
  };

  const onKey = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (open && (e.key === 'ArrowDown' || e.key === 'ArrowUp')) {
      e.preventDefault();
      setSel((s) => (s + (e.key === 'ArrowDown' ? 1 : -1) + matches.length) % matches.length);
      return;
    }
    if (open && e.key === 'Tab' && !e.shiftKey) {
      e.preventDefault();
      setText(picked.name + ' ');
      return;
    }
    if (e.key === 'Tab' && e.shiftKey) {
      e.preventDefault();
      cycleMode();
      return;
    }
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      submit(open ? picked.name : text);
      return;
    }
    if (e.key === 'Escape') {
      if (open) setText('');
      else interrupt();
    }
  };

  const stop = busy && !text.trim();
  return (
    <div className="px-md pt-sm border-border relative flex flex-col gap-1 border-t pb-[calc(0.5rem+env(safe-area-inset-bottom,0px))]">
      {open ? (
        <div
          role="listbox"
          aria-label="Commands"
          className="bg-term-bg-secondary border-term-border absolute right-0 bottom-full left-0 flex max-h-[46vh] flex-col overflow-y-auto border"
        >
          {matches.slice(0, 80).map((c, k) => (
            <button
              key={c.name}
              type="button"
              role="option"
              aria-selected={k === sel}
              onClick={() => {
                setText(c.name + ' ');
                ta.current?.focus();
              }}
              className={cn(
                'px-sm py-xs gap-sm grid grid-cols-[auto_1fr] items-baseline text-left',
                k === sel && 'bg-term-selection'
              )}
            >
              <span>{c.name}</span>
              <span className="text-term-fg-muted truncate text-xs">{c.description}</span>
            </button>
          ))}
        </div>
      ) : null}
      <div className="border-border focus-within:border-term-border pl-sm flex items-end gap-2 border p-1">
        <span className="text-term-accent pb-1" aria-hidden="true">
          ❯
        </span>
        <textarea
          ref={ta}
          rows={1}
          value={text}
          onChange={(e) => {
            setText(e.target.value);
            setSel(0);
          }}
          onKeyDown={onKey}
          placeholder={busy ? 'Queue a message' : 'Message, or / for commands'}
          aria-label="Message"
          className="placeholder:text-term-fg-muted max-h-[9em] min-w-0 flex-1 resize-none bg-transparent py-1 text-base leading-snug outline-none"
        />
        <button
          type="button"
          onClick={() => (stop ? interrupt() : submit())}
          aria-label={stop ? 'Interrupt' : 'Send'}
          className={cn('h-9 w-9 flex-none', stop ? 'text-term-error' : 'text-term-fg-muted')}
        >
          {stop ? '■' : '↵'}
        </button>
      </div>
      <div className="text-term-fg-muted gap-sm flex min-w-0 items-center text-xs">
        <button
          type="button"
          onClick={cycleMode}
          className={cn('min-h-8 flex-none', state.mode !== 'default' && 'text-term-magenta')}
        >
          ⏵⏵ {MODE_LABELS[state.mode] ?? state.mode}
        </button>
        <span className="truncate">{state.cwd}</span>
      </div>
    </div>
  );
}

function ModelSheet({
  state,
  send,
  close
}: {
  state: LiveState;
  send: LiveSessionViewProps['send'];
  close: () => void;
}) {
  useEffect(() => {
    const onKey = (e: globalThis.KeyboardEvent) => e.key === 'Escape' && close();
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [close]);
  const cur = state.models.find((m) => m.value === state.model);
  const efforts = cur?.supportedEffortLevels?.length ? cur.supportedEffortLevels : EFFORTS;
  return (
    <>
      <button
        type="button"
        aria-label="Close"
        onClick={close}
        className="bg-background/80 fixed inset-0 z-20"
      />
      <div
        role="dialog"
        aria-label="Model"
        className="bg-term-bg border-term-border p-md fixed inset-x-0 bottom-0 z-30 flex max-h-[80%] flex-col gap-3 overflow-y-auto border-t pb-[calc(1rem+env(safe-area-inset-bottom,0px))] font-mono text-sm md:right-6 md:bottom-6 md:left-auto md:w-[420px] md:border"
      >
        <h3 className="m-0 text-sm">Model · now {state.model || '…'}</h3>
        <div role="radiogroup" aria-label="Models" className="flex flex-col gap-1">
          {state.models.length ? (
            state.models.map((m) => (
              <button
                key={m.value}
                type="button"
                role="radio"
                aria-checked={m === cur}
                onClick={() => {
                  send({ k: 'model', model: m.value });
                  close();
                }}
                className={cn(
                  'border-border px-sm grid grid-cols-[1.5ch_1fr_auto] items-baseline gap-2 border py-2 text-left',
                  m === cur && 'border-term-fg bg-term-selection'
                )}
              >
                <span>{m === cur ? '❯' : ''}</span>
                <span>{m.displayName || m.value}</span>
                <span className="text-term-fg-muted text-xs">{m.value}</span>
                {m.description ? (
                  <span className="text-term-fg-muted col-span-2 col-start-2 text-xs">
                    {m.description}
                  </span>
                ) : null}
              </button>
            ))
          ) : (
            <p className="text-term-fg-muted text-xs">No model list yet.</p>
          )}
        </div>
        <div className="text-term-fg-muted text-xs tracking-widest uppercase">Effort</div>
        <div
          role="radiogroup"
          aria-label="Effort"
          className="border-term-border grid border"
          style={{ gridTemplateColumns: `repeat(${efforts.length}, 1fr)` }}
        >
          {efforts.map((x) => (
            <button
              key={x}
              type="button"
              role="radio"
              aria-checked={state.effort === x}
              onClick={() => {
                send({ k: 'effort', effort: x });
                close();
              }}
              className={cn(
                'border-border min-h-9 border-l text-xs first:border-l-0',
                state.effort === x && 'bg-term-selection'
              )}
            >
              {x}
            </button>
          ))}
        </div>
        <button type="button" onClick={close} className="border-border min-h-10 border">
          Close
        </button>
      </div>
    </>
  );
}
