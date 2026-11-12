import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import AccountTestModal from '../AccountTestModal.vue'
import AdminAccountTestModal from '@/components/admin/account/AccountTestModal.vue'
import { i18n, loadLocaleMessages } from '@/i18n'
import type { Account } from '@/types'

const { getAvailableModels } = vi.hoisted(() => ({ getAvailableModels: vi.fn() }))

vi.mock('@/api/admin', () => ({
  adminAPI: { accounts: { getAvailableModels } }
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({ copyToClipboard: vi.fn() })
}))

const account = {
  id: 42,
  name: 'Playground Fixture',
  platform: 'gemini',
  type: 'playground',
  status: 'active',
  credentials: {},
  extra: {}
} as Account

beforeEach(() => {
  getAvailableModels.mockResolvedValue([{ id: 'gemini-4-argon', display_name: 'Gemini 4 Argon' }])
  const events = new TextEncoder().encode(
    'data: {"type":"test_start","model":"gemini-4-argon"}\n\n' +
    'data: {"type":"test_complete","success":true}\n\n'
  )
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
    ok: true,
    body: {
      getReader: () => ({
        read: vi.fn()
          .mockResolvedValueOnce({ done: false, value: events })
          .mockResolvedValue({ done: true })
      })
    }
  }))
})

afterEach(() => vi.unstubAllGlobals())

describe.each([
  ['account', AccountTestModal],
  ['admin account', AdminAccountTestModal]
] as const)('%s test log interpolation', (_, component) => {
  it.each(['zh', 'en'] as const)('renders actual account and SSE model values in %s', async locale => {
    await loadLocaleMessages(locale)
    i18n.global.locale.value = locale
    const wrapper = mount(component, {
      props: { show: false, account },
      global: {
        plugins: [i18n],
        stubs: {
          BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' },
          Select: true,
          TextArea: true,
          Icon: true
        }
      }
    })
    await wrapper.setProps({ show: true })
    await flushPromises()
    const startButton = wrapper.findAll('button').find(button =>
      button.text() === (locale === 'zh' ? '开始测试' : 'Start Test')
    )
    expect(startButton).toBeDefined()
    expect(startButton!.attributes('disabled')).toBeUndefined()
    await startButton!.trigger('click')
    await flushPromises()

    const expected = locale === 'zh' ? [
      '开始测试账号：Playground Fixture',
      '账号类型：playground',
      '已连接到 API',
      '使用模型：gemini-4-argon',
      '发送测试消息："hi"'
    ] : [
      'Starting test for account: Playground Fixture',
      'Account type: playground',
      'Connected to API',
      'Using model: gemini-4-argon',
      'Sending test message: "hi"'
    ]
    for (const line of expected) expect(wrapper.text()).toContain(line)
    expect(wrapper.text()).not.toMatch(/\{(?:name|type|model)\}/)
    expect(fetch).toHaveBeenCalledOnce()
    wrapper.unmount()
  })
})
