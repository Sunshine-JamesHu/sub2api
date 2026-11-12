import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest'
import { ref } from 'vue'
import PlaygroundBatchLogin from '../PlaygroundBatchLogin.vue'

const mocks = vi.hoisted(() => ({list:vi.fn(),create:vi.fn(),action:vi.fn(),configure:vi.fn(),frame:vi.fn(),input:vi.fn(),finish:vi.fn(),getAll:vi.fn(),batchCreate:vi.fn()}))
vi.mock('@/i18n', () => ({ getLocale: () => 'zh-CN' }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ locale: ref('zh-CN'), t: (key: string) => key }) }))
vi.mock('@/api/admin/playgroundBatch', async importOriginal => ({...(await importOriginal<object>()), playgroundBatchAPI:mocks}))
vi.mock('@/api/admin/proxies', () => ({default:{getAll:mocks.getAll,batchCreate:mocks.batchCreate}}))
const BaseDialog = {props:['show','title'],template:'<section v-if="show"><h2>{{ title }}</h2><slot /></section>'}
let wrapper: ReturnType<typeof mount> | undefined
const state = (batches: unknown[] = []) => ({concurrency:20,running:0,capacity:{configured:20,effective:2,active:0,waiting:0},batches})
const job = {id:'job-1',email:'owner@example.test',status:'manual',step:'manual',reason:'需要人机验证',proxy_id:1,proxy_name:'http://198.51.100.1:8080',attempts:1,has_credentials:true,manual_open:false,opening:false,updated:'2026-11-06T00:00:00Z'}
const batch = (id='batch-1', jobs=[job]) => ({id,paused:false,created:'2026-11-06T00:00:00Z',proxy_ids:[1],group_ids:[],auto_onboarding:false,counts:{manual:jobs.length},jobs})
const button = (text:string) => wrapper!.findAll('button').find(node => node.text() === text)!
beforeEach(() => {vi.useFakeTimers();vi.clearAllMocks();mocks.list.mockResolvedValue(state());mocks.getAll.mockResolvedValue([{id:1,protocol:'http',host:'198.51.100.1',port:8080,status:'active',expires_at:null}]);mocks.create.mockResolvedValue({id:'batch-1'});mocks.action.mockResolvedValue({});mocks.frame.mockResolvedValue({image:'synthetic',live:true})})
afterEach(async()=>{wrapper?.unmount();await flushPromises();vi.clearAllTimers();vi.useRealTimers()})
const group = {id:7,name:'Gemini pool',platform:'gemini',status:'active'} as any
async function open(groups = [group]){wrapper=mount(PlaygroundBatchLogin,{props:{show:true,groups},global:{stubs:{BaseDialog}}});await flushPromises()}

describe('Playground batch sign-in',()=>{
  it('submits the selected Gemini website source for batch login', async () => {
    await open()
    await wrapper!.get('#batch-backend').setValue('gemini_web')
    await wrapper!.get('#batch-accounts').setValue('owner@example.test----synthetic----')
    await wrapper!.get('form').trigger('submit')
    await flushPromises()
    expect(mocks.create).toHaveBeenCalledWith(expect.objectContaining({ backend: 'gemini_web' }))
  })
  it('ignores a list response from a previous dialog opening', async () => {
    let resolveOld!: (value: ReturnType<typeof state>) => void
    mocks.list.mockImplementationOnce(() => new Promise(resolve => { resolveOld = resolve }))
    wrapper = mount(PlaygroundBatchLogin, { props: { show: true, groups: [group] }, global: { stubs: { BaseDialog } } })
    await flushPromises()
    await wrapper.setProps({ show: false })
    mocks.list.mockResolvedValue(state([batch('batch-2')]))
    await wrapper.setProps({ show: true })
    await flushPromises()
    resolveOld(state([batch('stale-batch')]))
    await flushPromises()
    expect((wrapper.get('select[aria-label="选择批次"]').element as HTMLSelectElement).value).toBe('batch-2')
    expect(wrapper.html()).not.toContain('stale-batch')
  })
  it('ignores a screenshot returned after switching away from its batch', async () => {
    let resolveFrame!: (value: { image: string; live: boolean }) => void
    mocks.list.mockResolvedValue(state([batch(), batch('batch-2', [])]))
    mocks.frame.mockImplementationOnce(() => new Promise(resolve => { resolveFrame = resolve }))
    await open()
    await button('人工处理').trigger('click')
    await flushPromises()
    await wrapper!.get('select[aria-label="选择批次"]').setValue('batch-2')
    await flushPromises()
    resolveFrame({ image: 'stale-frame-marker', live: true })
    await flushPromises()
    expect(wrapper!.html()).not.toContain('stale-frame-marker')
    await vi.advanceTimersByTimeAsync(1400)
    expect(mocks.frame).toHaveBeenCalledTimes(1)
  })
  it('requires proxies and does not submit a direct login',async()=>{mocks.getAll.mockResolvedValue([]);await open();await wrapper!.get('#batch-accounts').setValue('owner@example.test----synthetic----');expect(button('导入并开始').attributes('disabled')).toBeDefined();expect(mocks.create).not.toHaveBeenCalled()})
  it('submits selected proxies and concurrency, then removes the credential input',async()=>{await open();await wrapper!.get('#batch-accounts').setValue('owner@example.test----private-password-marker----JBSWY3DPEHPK3PXP');mocks.list.mockResolvedValue(state([batch('batch-1',[])]));await wrapper!.get('form').trigger('submit');await flushPromises();expect(mocks.create).toHaveBeenCalledWith(expect.objectContaining({concurrency:20,proxy_ids:[1],group_ids:[7],auto_onboarding:true}));expect(wrapper!.html()).not.toContain('private-password-marker');await button('新建批次').trigger('click');expect((wrapper!.get('#batch-accounts').element as HTMLTextAreaElement).value).toBe('')})
  it('closes the original batch browser when switching batches',async()=>{mocks.list.mockResolvedValue(state([batch(),batch('batch-2',[])]));await open();await button('人工处理').trigger('click');await flushPromises();expect(mocks.action).toHaveBeenCalledWith('batch-1','open_manual','job-1');await wrapper!.get('select[aria-label="选择批次"]').setValue('batch-2');await flushPromises();expect(mocks.action).toHaveBeenCalledWith('batch-1','close_manual','job-1');expect(mocks.action).not.toHaveBeenCalledWith('batch-2','close_manual','job-1')})
  it('does not send keyboard actions to an archived screenshot',async()=>{mocks.list.mockResolvedValue(state([batch()]));mocks.frame.mockResolvedValue({image:'synthetic',live:false});await open();await button('人工处理').trigger('click');await flushPromises();await wrapper!.get('[role="group"]').trigger('keydown',{key:'a'});expect(mocks.input).not.toHaveBeenCalled();expect(button('完成并保存账号').attributes('disabled')).toBeDefined()})
  it('requires a deliberate choice when several groups exist',async()=>{await open([group,{...group,id:8,name:'Other pool'}]);await wrapper!.get('#batch-accounts').setValue('owner@example.test----synthetic----');expect(button('导入并开始').attributes('disabled')).toBeDefined();await wrapper!.get('form').trigger('submit');expect(mocks.create).not.toHaveBeenCalled();await wrapper!.get('input[data-group-id="8"]').setValue(true);await wrapper!.get('form').trigger('submit');await flushPromises();expect(mocks.create).toHaveBeenCalledWith(expect.objectContaining({group_ids:[8]}))})
  it('allows unassigned imports only after explicitly opting in',async()=>{await open([]);await wrapper!.get('#batch-accounts').setValue('owner@example.test----synthetic----');await wrapper!.get('form').trigger('submit');expect(mocks.create).not.toHaveBeenCalled();expect(wrapper!.text()).toContain('未分组的账号不会被 API 请求选中');await wrapper!.get('#batch-unassigned').setValue(true);await wrapper!.get('form').trigger('submit');await flushPromises();expect(mocks.create).toHaveBeenCalledWith(expect.objectContaining({group_ids:[]}))})
  it('selects a sole asynchronously loaded group but preserves a user-cleared selection',async()=>{await open([]);await wrapper!.setProps({groups:[group]});expect((wrapper!.get('input[data-group-id="7"]').element as HTMLInputElement).checked).toBe(true);await wrapper!.get('input[data-group-id="7"]').setValue(false);await wrapper!.setProps({groups:[{...group}]});expect((wrapper!.get('input[data-group-id="7"]').element as HTMLInputElement).checked).toBe(false)})

})
