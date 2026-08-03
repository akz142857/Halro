export const MIN_PASSWORD_CHARACTERS = 8;
export const MAX_PASSWORD_BYTES = 1024;

export function passwordCharacterCount(value: string) {
  return Array.from(value).length;
}

export function passwordByteCount(value: string) {
  return new TextEncoder().encode(value).length;
}
