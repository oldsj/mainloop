import type { components } from '@/api/schema.gen';

type Schemas = components['schemas'];

export type MainThreadInfo = Schemas['MainThreadInfo'];
export type NativeSessionInfo = Schemas['NativeSessionInfo'];
export type NativeDeliveryInfo = Schemas['NativeDeliveryInfo'];
export type ChatMessage = Schemas['Message'];
