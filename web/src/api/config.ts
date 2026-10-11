/**
 * Base URL of the Mainloop API. Dev and MSW use `/api` (Vite forwards it with the prefix removed);
 * the image build bakes an absolute URL in through the VITE_API_URL build arg.
 */
export const API_URL: string = (import.meta.env.VITE_API_URL || '/api').replace(/\/+$/, '');
