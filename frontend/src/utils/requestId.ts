let fallbackSequence = 0

/** Generate an operation ID, including on HTTP pages without randomUUID. */
export function createRequestId(): string {
  const provider = globalThis.crypto
  try {
    if (typeof provider?.randomUUID === 'function') return provider.randomUUID()
  } catch {
    // Some browser shims expose the method but reject calls outside HTTPS.
  }
  try {
    if (typeof provider?.getRandomValues === 'function') {
      const bytes = provider.getRandomValues(new Uint8Array(16))
      bytes[6] = (bytes[6]! & 0x0f) | 0x40
      bytes[8] = (bytes[8]! & 0x3f) | 0x80
      const hex = Array.from(bytes, value => value.toString(16).padStart(2, '0')).join('')
      return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
    }
  } catch {
    // Operation IDs can still be generated when Web Crypto is unavailable.
  }
  return `${Date.now()}-${++fallbackSequence}-${Math.random().toString(36).slice(2)}`
}
