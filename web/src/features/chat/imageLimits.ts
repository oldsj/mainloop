import type { components } from '@/api/schema.gen';

/** The formats the backend's upload endpoint accepts, per the contract's enum. */
export type AllowedImageType = components['schemas']['ChatImageUpload']['media_type'];

/**
 * Pasted-image constraints for the main chat composer, matching Anthropic's Messages API vision
 * limits: these are the real provider limits, not arbitrary ones, so the client refuses early
 * with the same reason the backend would.
 */
export const MAX_IMAGE_BYTES = 5 * 1024 * 1024;

const ALLOWED_IMAGE_TYPES: ReadonlySet<string> = new Set([
  'image/png',
  'image/jpeg',
  'image/webp',
  'image/gif'
]);

export function isAllowedImageType(type: string): type is AllowedImageType {
  return ALLOWED_IMAGE_TYPES.has(type);
}

/** One image attached to the composer, read into memory as base64 (no `data:` prefix). */
export interface PendingImage {
  mediaType: AllowedImageType;
  data: string;
}

/**
 * Reads a file's bytes as base64, for both the composer's preview and the upload body. The
 * caller has already checked `file.type` with `isAllowedImageType`, which is what makes it an
 * `AllowedImageType` here.
 */
export function fileToPendingImage(file: File, mediaType: AllowedImageType): Promise<PendingImage> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => {
      const result = reader.result;
      if (typeof result !== 'string') {
        reject(new Error('Could not read the image'));
        return;
      }
      const comma = result.indexOf(',');
      resolve({ mediaType, data: comma >= 0 ? result.slice(comma + 1) : result });
    };
    reader.onerror = () => reject(reader.error ?? new Error('Could not read the image'));
    reader.readAsDataURL(file);
  });
}

/** A `data:` URL for showing a pending or sent image inline. */
export function imageDataUrl(image: { mediaType: string; data: string }): string {
  return `data:${image.mediaType};base64,${image.data}`;
}
