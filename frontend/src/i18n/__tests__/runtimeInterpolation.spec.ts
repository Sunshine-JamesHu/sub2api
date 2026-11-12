import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import AccountStatusIndicator from '@/components/account/AccountStatusIndicator.vue'
import { i18n, loadLocaleMessages } from '@/i18n'
import type { Account } from '@/types'
import { formatCountdown, formatCountdownWithSuffix } from '@/utils/format'

const now = new Date('2026-11-09T00:00:00Z')
const afterMinutes = (minutes: number) => new Date(now.getTime() + minutes * 60_000).toISOString()

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe.each(['zh', 'en'] as const)('%s runtime interpolation', locale => {
  beforeEach(async () => {
    vi.useFakeTimers()
    vi.setSystemTime(now)
    await loadLocaleMessages(locale)
    i18n.global.locale.value = locale
  })

  it.each([
    [15, '15m'],
    [161, '2h 41m'],
    [3 * 24 * 60 + 5 * 60, '3d 5h']
  ])('formats a countdown of %i minutes', (minutes, expected) => {
    expect(formatCountdown(afterMinutes(minutes))).toBe(expected)
  })

  it('interpolates the countdown inside its translated suffix', () => {
    expect(formatCountdownWithSuffix(afterMinutes(161))).toBe(
      locale === 'zh' ? '2h 41m 后解除' : '2h 41m to lift'
    )
  })

  it('renders the Argon rate limit with actual remaining time', () => {
    const wrapper = mount(AccountStatusIndicator, {
      props: {
        account: {
          platform: 'gemini',
          type: 'playground',
          status: 'active',
          schedulable: true,
          extra: {
            model_rate_limits: {
              'gemini-4-argon': {
                rate_limited_at: now.toISOString(),
                rate_limit_reset_at: afterMinutes(161)
              }
            }
          }
        } as Account
      },
      global: { plugins: [i18n], stubs: { Icon: true } }
    })
    expect(wrapper.text()).toContain('gemini-4-argon')
    expect(wrapper.text()).toContain('2h 41m')
    expect(wrapper.text()).not.toMatch(/\{[hm]\}/)
    wrapper.unmount()
  })

  it('compiles a fresh parameterized message without unsafe-eval', () => {
    vi.stubGlobal('Function', function () {
      throw new Error('unsafe-eval is blocked by CSP')
    })
    expect(i18n.global.t('common.selectedCount', { count: 7 })).toBe(
      locale === 'zh' ? '（已选 7 个）' : '(7 selected)'
    )
  })
})
