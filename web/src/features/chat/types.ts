import type { components } from '@/api/schema.gen';

type Schemas = components['schemas'];

export type MainThreadInfo = Schemas['MainThreadInfo'];
export type NativeSessionInfo = Schemas['NativeSessionInfo'];
export type NativeDeliveryInfo = Schemas['NativeDeliveryInfo'];
export type ChatMessage = Schemas['Message'];
export type MainProvider = Schemas['MainProvider'];
export type MainProviderInfo = Schemas['MainProviderInfo'];
export type MainProviderSelection = Schemas['MainProviderSelection'];
/** The stable code a refused main-provider switch or pending-message send carries (absent otherwise). */
export type MainProviderRefusalCode = NonNullable<Schemas['CodedHTTPError']['code']>;
