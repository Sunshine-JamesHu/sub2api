import { afterEach, describe, expect, it, vi } from 'vitest'
import { createRequestId } from '../requestId'

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('createRequestId', () => {
  it('uses the native method with its crypto receiver', () => {
    const provider = { randomUUID() {
      expect(this).toBe(provider)
      return '11111111-1111-4111-8111-111111111111'
    } }
    vi.stubGlobal('crypto', provider)
    expect(createRequestId()).toBe('11111111-1111-4111-8111-111111111111')
  })

  it.each([undefined, 'unavailable', () => { throw new Error('HTTPS required') }])(
    'uses getRandomValues when randomUUID is unavailable or unusable (%s)', randomUUID => {
      vi.stubGlobal('crypto', {
        randomUUID,
        getRandomValues: (bytes: Uint8Array) => bytes.fill(0xab)
      })
      expect(createRequestId()).toBe('abababab-abab-4bab-abab-abababababab')
    }
  )

  it.each([undefined, {}, { getRandomValues: () => { throw new Error('unavailable') } }])(
    'keeps operation IDs distinct without Web Crypto (%s)', crypto => {
      vi.stubGlobal('crypto', crypto)
      vi.spyOn(Date, 'now').mockReturnValue(1000)
      vi.spyOn(Math, 'random').mockReturnValue(0.5)
      const first = createRequestId()
      expect(first).toBeTruthy()
      expect(createRequestId()).not.toBe(first)
    }
  )
})
