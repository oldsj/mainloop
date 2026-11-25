import { useState } from 'react';
import { ApiError } from '@/api/errors';
import type { components } from '@/api/schema.gen';
import { ConfirmDialog } from '@/components/shared/confirm-dialog';
import { ErrorRecovery } from '@/components/shared/error-recovery';
import { StatusBadge } from '@/components/shared/status-badge';
import { Button } from '@/components/ui/button';
import { messageTime } from '@/lib/time';
import { useDevices, useRevokeDevice } from './queries';

type Device = components['schemas']['Device'];

/** The server returns at most this many devices, newest first. */
const DEVICE_LIMIT = 100;

const PLATFORM_LABEL: Record<Device['platform'], string> = { ios: 'iOS' };

/** The start of the key's thumbprint: enough to tell two keys apart at a glance. */
const shortThumbprint = (thumbprint: string) =>
  thumbprint.length > 12 ? `${thumbprint.slice(0, 12)}…` : thumbprint;

/** Whether Mainloop can push to the device (docs/specs/push-notifications.md). */
function pushFact(push: Device['push']): string {
  if (push === null) return 'notifications off';
  if (push.invalid_at) return 'notifications stopped: Apple refused its token';
  return 'notifications on';
}

/**
 * The device's approval key as the chart pins it (executor.approvalKeys.pinned, see
 * docs/specs/privileged-actions.md): one list entry, the public JWK with the device id as kid.
 */
export function pinEntry(device: Device): string | null {
  const key = device.approval_key;
  if (key === null) return null;
  const { kty, crv, x, y } = key.jwk;
  return `- {kid: ${device.id}, kty: ${kty}, crv: ${crv}, x: ${x}, y: ${y}}`;
}

/** What a failed revoke says. The server's own sentence when it sent one. */
function revokeError(error: unknown): string {
  if (error instanceof ApiError && error.status === 404)
    return 'Not revoked: Mainloop has no such device.';
  if (error instanceof ApiError && error.message) return `Not revoked: ${error.message}`;
  return 'Mainloop did not confirm the revoke. Try again; revoking twice changes nothing.';
}

/**
 * Every phone registered to call the API as the owner, revoked ones included, so the owner can see
 * a device they did not register and stop its key. Revoking cannot be undone.
 */
export function DevicesSection() {
  const devices = useDevices();
  const revoke = useRevokeDevice();
  const [confirming, setConfirming] = useState<Device | null>(null);
  // The last failed revoke, shown on its device's row until the next one starts.
  const [failure, setFailure] = useState<{ id: string; text: string } | null>(null);

  function revokeConfirmed() {
    if (!confirming) return;
    const id = confirming.id;
    setFailure(null);
    revoke.mutate(
      { params: { path: { device_id: id } } },
      {
        onSuccess: () => setConfirming(null),
        onError: (error) => {
          setConfirming(null);
          setFailure({ id, text: revokeError(error) });
        }
      }
    );
  }

  return (
    <section aria-labelledby="devices-heading" className="gap-md flex flex-col">
      <div className="gap-xs flex flex-col">
        <h2 id="devices-heading" className="text-base font-semibold">
          Devices
        </h2>
        <p className="text-muted-foreground max-w-measure text-sm">
          Phones that call Mainloop as you, each with a key that never leaves it. Revoke one you do
          not recognise or no longer use: its key stops working on its next request.
        </p>
      </div>

      {devices.isError ? (
        <ErrorRecovery
          title="Could not load the devices"
          message="Nothing was changed."
          pending={devices.isFetching}
          onRetry={() => void devices.refetch()}
        />
      ) : devices.data === undefined ? (
        <p role="status" className="text-muted-foreground text-sm">
          Loading devices…
        </p>
      ) : devices.data.length === 0 ? (
        <p className="text-muted-foreground text-sm">
          No devices registered. Sign in from the iPhone app’s Settings to register one.
        </p>
      ) : (
        <>
          <ul
            aria-label="Registered devices"
            className="border-border-subtle divide-border-subtle divide-y border"
          >
            {devices.data.map((device) => (
              <DeviceRow
                key={device.id}
                device={device}
                busy={revoke.isPending}
                error={failure?.id === device.id ? failure.text : null}
                onRevoke={() => setConfirming(device)}
              />
            ))}
          </ul>
          {devices.data.length >= DEVICE_LIMIT ? (
            <p className="text-muted-foreground text-xs">
              Showing the newest {DEVICE_LIMIT} devices.
            </p>
          ) : null}
        </>
      )}

      <ConfirmDialog
        open={confirming !== null}
        onOpenChange={(open) => {
          if (!open) setConfirming(null);
        }}
        title={`Revoke ${confirming?.name ?? 'this device'}?`}
        description="Its key stops working at once, and the phone must sign in again to register a new one. A revoked device cannot be restored."
        confirmLabel="Revoke"
        destructive
        pending={revoke.isPending}
        onConfirm={revokeConfirmed}
      />
    </section>
  );
}

function DeviceRow({
  device,
  busy,
  error,
  onRevoke
}: {
  device: Device;
  busy: boolean;
  error: string | null;
  onRevoke: () => void;
}) {
  const revokedAt = device.revoked_at;
  const pin = pinEntry(device);
  const facts = [
    PLATFORM_LABEL[device.platform],
    `registered ${messageTime(device.created_at)}`,
    `last seen ${device.last_seen_at ? messageTime(device.last_seen_at) : 'never'}`,
    revokedAt ? `revoked ${messageTime(revokedAt)}` : pushFact(device.push)
  ].filter((fact): fact is string => fact !== null);

  return (
    <li className="gap-xs p-sm flex flex-col text-sm" data-testid="device">
      <div className="gap-sm flex flex-wrap items-center">
        <p className="font-medium break-words">{device.name}</p>
        {device.current ? <span className="text-muted-foreground text-xs">this device</span> : null}
        <StatusBadge label={revokedAt ? 'Revoked' : 'Active'} tone={revokedAt ? 'muted' : 'ok'} />
      </div>
      <p className="text-muted-foreground text-xs">{facts.join(' · ')}</p>
      <p className="text-muted-foreground text-xs break-all">
        {device.id} · key{' '}
        <span title={device.key_thumbprint}>{shortThumbprint(device.key_thumbprint)}</span>
        {device.approval_key ? (
          <>
            {' · Touch ID approval key '}
            <span title={device.approval_key.thumbprint}>
              {shortThumbprint(device.approval_key.thumbprint)}
            </span>
          </>
        ) : (
          ' · no approval key'
        )}
      </p>
      {revokedAt || pin === null ? null : (
        <details className="text-xs">
          <summary className="text-muted-foreground cursor-pointer">
            Pin its approval key for the executor
          </summary>
          <p className="text-muted-foreground mt-xs max-w-measure">
            Add this entry to <code>executor.approvalKeys.pinned</code> in the chart values. The
            executor then runs only actions this phone approved with Touch ID. It is a public key.
          </p>
          <pre className="bg-muted p-xs mt-xs break-all whitespace-pre-wrap">
            <code>{pin}</code>
          </pre>
        </details>
      )}
      {revokedAt ? null : (
        <div>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={busy}
            aria-label={`Revoke ${device.name}`}
            onClick={onRevoke}
          >
            Revoke
          </Button>
        </div>
      )}
      {error ? (
        <p role="alert" className="text-destructive text-xs">
          {error}
        </p>
      ) : null}
    </li>
  );
}
