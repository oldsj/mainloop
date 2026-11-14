import { useState } from 'react';
import { ControlsHeading, SessionControls } from '@/components/shared/controls-sheet';
import { Button } from '@/components/ui/button';
import { cn } from '@/components/ui/cn';
import {
  MAIN_PROVIDERS,
  providerLabel,
  selectionIsNoop,
  type MainProviderControl
} from './mainProvider';
import type { MainProvider, MainThreadInfo } from './types';

/** The chip's word for the main provider: the one the owner selected, or that it is not known. */
export function providerChipLabel(info: MainThreadInfo): string {
  const desired = info.provider?.desired_provider;
  return desired ? providerLabel(desired) : 'provider unknown';
}

/** The model in use, without its vendor prefix; null when the session reports none. */
export function modelInUse(info: MainThreadInfo): string | null {
  return info.native?.model ? info.native.model.replace(/^claude-/, '') : null;
}

export interface ProviderChoiceProps {
  info: MainThreadInfo;
  provider: MainProviderControl;
  /** Called after the owner confirms a switch, so the picker can close. */
  onDone?: () => void;
}

/**
 * The owner's choice of main provider, as the first section of the session controls picker.
 * Switching starts a fresh native session, so a choice is confirmed before anything is sent.
 */
export function ProviderChoice({ info, provider, onDone }: ProviderChoiceProps) {
  const desired: MainProvider | null = info.provider?.desired_provider ?? null;
  const [asking, setAsking] = useState<MainProvider | null>(null);
  const held = provider.switchBlocked;
  const confirming = held ? null : asking;

  return (
    <section className="flex flex-col gap-2" data-testid="provider-choice">
      <ControlsHeading>Provider</ControlsHeading>
      <div
        role="radiogroup"
        aria-label="Main provider"
        className="border-border-subtle grid grid-cols-2 border"
      >
        {MAIN_PROVIDERS.map((option) => (
          <button
            key={option.value}
            type="button"
            role="radio"
            aria-checked={desired === option.value}
            disabled={held}
            onClick={() =>
              setAsking(selectionIsNoop(option.value, info.provider) ? null : option.value)
            }
            className={cn(
              'border-border-subtle hover:bg-accent min-h-control border-l text-xs first:border-l-0 disabled:pointer-events-none disabled:opacity-50',
              desired === option.value && 'bg-term-selection'
            )}
            data-testid={`provider-option-${option.value}`}
          >
            {option.label}
          </button>
        ))}
      </div>
      {held ? (
        <p className="text-muted-foreground text-xs" data-testid="provider-held">
          The provider cannot change right now. It is held while the main thread has open work, a
          switch is settling, or the app is offline.
        </p>
      ) : null}
      {confirming ? (
        <div
          role="group"
          aria-label={`Switch to ${providerLabel(confirming)}`}
          className="border-status-attention/50 p-sm gap-sm flex flex-col border"
          data-testid="provider-confirm"
        >
          <p className="text-xs">
            Switching to {providerLabel(confirming)} starts a fresh native session. Your
            conversation, tasks and earlier native history are kept.
          </p>
          <div className="gap-sm flex flex-wrap">
            <Button
              size="sm"
              onClick={() => {
                provider.select(confirming);
                setAsking(null);
                onDone?.();
              }}
              data-testid="provider-confirm-switch"
            >
              Switch to {providerLabel(confirming)}
            </Button>
            <Button variant="ghost" size="sm" onClick={() => setAsking(null)}>
              Cancel
            </Button>
          </div>
        </div>
      ) : null}
    </section>
  );
}

/**
 * The session controls for the chat view of the main thread: a chip naming the provider and the
 * model in use, which opens the provider choice. Model and effort are slash commands there.
 */
export function ChatControls({ info, provider }: Omit<ProviderChoiceProps, 'onDone'>) {
  const [open, setOpen] = useState(false);
  const model = modelInUse(info);
  const name = providerChipLabel(info);
  const label = model ? `${name} · ${model}` : name;
  return (
    <SessionControls
      label={label}
      title="Session controls"
      open={open}
      onOpenChange={setOpen}
      data-testid="session-controls"
    >
      <ProviderChoice info={info} provider={provider} onDone={() => setOpen(false)} />
      <p className="text-muted-foreground text-xs">
        Model and effort: type <code>/model</code> or <code>/effort</code>.
      </p>
    </SessionControls>
  );
}
