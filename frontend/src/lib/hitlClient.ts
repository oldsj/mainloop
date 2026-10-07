import { api } from './api';
import { createHITLChannel, type HITLState } from './hitl';
import { visiblePolling } from './visiblePolling';

export const hitlClient = createHITLChannel(api.getHITL, api.respondHITL);
export function watchHITL(id: string, listener: (state: HITLState) => void) {
  const unsubscribe = hitlClient.subscribe(id, listener);
  const stop = visiblePolling().watch(`hitl:${id}`, (signal) => hitlClient.poll(id, signal));
  return () => {
    stop();
    unsubscribe();
  };
}
