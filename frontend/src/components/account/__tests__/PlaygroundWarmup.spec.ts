import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest'
import { ref } from 'vue'
import PlaygroundWarmup from '../PlaygroundWarmup.vue'
const api = vi.hoisted(() => ({ get:vi.fn(), configure:vi.fn() }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ locale:ref('zh-CN') }) }))
vi.mock('@/api/admin/playgroundWarmup', () => ({ playgroundWarmupAPI:api }))
const BaseDialog = { props:['show','title'], template:'<section v-if="show"><button data-test="close" @click="$emit(\'close\')">关闭</button><slot /></section>' }
const fixture = () => ({ config:{ enabled:true,target:8,max_cached:16,idle_minutes:30 },eligible:20,ready:8,cached:8,light_cached:2,active_requests:0,memory_headroom_mb:900,accounts:[] })
let wrapper:ReturnType<typeof mount>
beforeEach(() => { vi.useFakeTimers();vi.clearAllMocks();api.get.mockResolvedValue(fixture());api.configure.mockImplementation(async config => ({...fixture(),config}));wrapper=mount(PlaygroundWarmup,{global:{stubs:{BaseDialog}}}) })
afterEach(() => {wrapper.unmount();vi.clearAllTimers();vi.useRealTimers()})
async function open() {await wrapper.get('button').trigger('click');await flushPromises()}
describe('Playground prewarm controls', () => {
 it('polls only while open and preserves unsaved settings', async () => {
  expect(api.get).not.toHaveBeenCalled();await open()
  await wrapper.get('[data-test="warmup-target"]').setValue(12)
  await vi.advanceTimersByTimeAsync(5000);await flushPromises()
  expect((wrapper.get('[data-test="warmup-target"]').element as HTMLInputElement).value).toBe('12')
  await wrapper.get('[data-test="close"]').trigger('click')
  const calls=api.get.mock.calls.length;await vi.advanceTimersByTimeAsync(15000);expect(api.get).toHaveBeenCalledTimes(calls)
 })
 it('validates the target against the cache budget and saves custom settings', async () => {
  await open();await wrapper.get('[data-test="warmup-target"]').setValue(20)
  await wrapper.get('form').trigger('submit');expect(api.configure).not.toHaveBeenCalled()
  await wrapper.get('[data-test="warmup-cache"]').setValue(24)
  await wrapper.get('form').trigger('submit');await flushPromises()
  expect(api.configure).toHaveBeenCalledWith({enabled:true,target:20,max_cached:24,idle_minutes:30})
  expect(wrapper.text()).toContain('不是请求并发上限')
 })
})
