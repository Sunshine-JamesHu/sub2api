import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { AxiosInstance } from 'axios'

vi.mock('@/i18n', () => ({ getLocale: () => 'zh-CN' }))

describe('account archive transfers', () => {
  let client: AxiosInstance
  beforeEach(async () => {
    vi.resetModules()
    localStorage.clear()
    client = (await import('@/api/client')).apiClient
  })

  const binary = (text: string) => new TextEncoder().encode(text).buffer

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('downloads ZIP and preserves skipped-account warnings', async () => {
    client.defaults.adapter = async (config) => ({
      config, status: 200, statusText: 'OK', data: binary('PK-synthetic'),
      headers: { 'content-type': 'application/zip', 'x-skipped-shadows': '2' }
    })
    const result = await (await import('@/api/admin/accounts')).exportData()
    expect(result.archive).toBeInstanceOf(Blob)
    expect(result.archive?.type).toBe('application/zip')
    expect(result.skipped_shadows).toBe(2)
  })

  it('retains ordinary JSON downloads', async () => {
    const payload = { exported_at: 'test', accounts: [], proxies: [] }
    client.defaults.adapter = async (config) => ({
      config, status: 200, statusText: 'OK', data: binary(JSON.stringify({ code: 0, data: payload })),
      headers: { 'content-type': 'application/json' }
    })
    expect(await (await import('@/api/admin/accounts')).exportData()).toEqual(payload)
  })

  it('decodes step-up errors before error interceptors', async () => {
    client.defaults.adapter = async (config) => {
      throw { config, response: { status: 403, config,
        data: binary(JSON.stringify({ code: 403, reason: 'STEP_UP_REQUIRED', message: 'Verify identity' })),
        headers: { 'content-type': 'application/json' } } }
    }
    await expect((await import('@/api/admin/accounts')).exportData()).rejects.toMatchObject({
      status: 403, reason: 'STEP_UP_REQUIRED', message: 'Verify identity'
    })
  })

  it('uploads multipart with a stable key across a failed request retry', async () => {
    const file = new File(['PK-synthetic'], 'accounts.zip', { type: 'application/zip' })
    const keys: string[] = []
    client.defaults.adapter = async (config) => {
      expect(config.data).toBeInstanceOf(FormData)
      expect(config.data.get('file')).toBe(file)
      keys.push(config.headers.get('Idempotency-Key') as string)
      if (keys.length === 1) throw new Error('network failure')
      return { config, status: 200, statusText: 'OK', headers: {}, data: { code: 0, data: { account_created: 1 } } }
    }
    const { importArchive } = await import('@/api/admin/accounts')
    await expect(importArchive(file)).rejects.toBeDefined()
    expect(await importArchive(file)).toMatchObject({ account_created: 1 })
    expect(keys[0]).toBeTruthy()
    expect(keys[1]).toBe(keys[0])
  })

  it.each([
    ['randomUUID unavailable', { randomUUID: undefined }],
    ['crypto unavailable', undefined],
  ])('imports archives with %s and preserves retry keys', async (_, crypto) => {
    vi.stubGlobal('crypto', crypto)
    const keys: string[] = []
    client.defaults.adapter = async (config) => {
      keys.push(config.headers.get('Idempotency-Key') as string)
      if (keys.length === 1) throw new Error('network failure')
      return { config, status: 200, statusText: 'OK', headers: {}, data: { code: 0, data: { account_created: 1 } } }
    }
    const { importArchive } = await import('@/api/admin/accounts')
    const file = new File(['PK-synthetic'], 'accounts.zip', { type: 'application/zip' })
    await expect(importArchive(file)).rejects.toBeDefined()
    expect(await importArchive(file)).toMatchObject({ account_created: 1 })
    expect(keys[0]).toMatch(/^account-archive-.+/)
    expect(keys[1]).toBe(keys[0])
    const nextFile = new File(['PK-synthetic'], 'next.zip', { type: 'application/zip' })
    await importArchive(nextFile)
    expect(keys[2]).not.toBe(keys[0])
  })
})
