import { DeliveryNotice } from './DeliveryNotice';

/** A message that was not sent (with Retry), a failed one with no Retry, and an unconfirmed one. */
export default function DeliveryNoticePreview() {
  return (
    <div className="p-md gap-sm flex flex-col">
      <DeliveryNotice
        notice={{
          state: 'failed',
          label: 'Not sent',
          reason: 'not sent: the agent is not running',
          retryable: true
        }}
        onRetry={() => {}}
      />
      <DeliveryNotice
        notice={{
          state: 'failed',
          label: 'Failed',
          reason: 'agent error: out of memory',
          retryable: false
        }}
      />
      <DeliveryNotice
        notice={{
          state: 'uncertain',
          label: 'Delivery unconfirmed',
          reason: 'timed out waiting for the agent',
          retryable: false
        }}
      />
    </div>
  );
}
