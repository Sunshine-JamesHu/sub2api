<template>
  <BaseDialog :show="show" :title="l.title" width="extra-wide" :close-on-escape="!manualId" @close="close">
    <div class="space-y-5">
      <p class="text-sm text-gray-600 dark:text-gray-300">{{ l.intro }}</p>
      <p v-if="error || state?.error" role="alert" class="rounded-lg bg-red-50 p-3 text-sm text-red-700 dark:bg-red-900/20">{{ error || state?.error }}</p>
      <p v-if="notice" role="status" class="rounded-lg bg-green-50 p-3 text-sm text-green-700 dark:bg-green-900/20">{{ notice }}</p>

      <div class="grid grid-cols-2 gap-3 md:grid-cols-4" aria-live="polite">
        <div class="rounded-lg border p-3 dark:border-dark-600"><div class="text-xs text-gray-500">{{ l.running }}</div><strong class="text-xl">{{ state?.running || 0 }}</strong></div>
        <div class="rounded-lg border p-3 dark:border-dark-600"><div class="text-xs text-gray-500">{{ l.resourceSlots }}</div><strong class="text-xl">{{ state?.capacity.effective ?? '—' }}</strong></div>
        <div class="rounded-lg border p-3 dark:border-dark-600"><div class="text-xs text-gray-500">{{ l.manual }}</div><strong class="text-xl">{{ totalCount('manual') }}</strong></div>
        <div class="rounded-lg border p-3 dark:border-dark-600"><div class="text-xs text-gray-500">{{ l.saved }}</div><strong class="text-xl">{{ totalCount('succeeded') }}</strong></div>
      </div>
      <div class="flex flex-wrap items-center gap-3">
        <label class="text-sm" for="batch-target">{{ l.concurrency }}</label>
        <input id="batch-target" v-model.number="concurrency" type="number" min="1" max="128" class="input w-24" />
        <button class="btn btn-secondary" :disabled="busy" @click="applyConcurrency">{{ l.apply }}</button>
        <button class="btn btn-primary ml-auto" @click="showForm = !showForm">{{ showForm ? l.hideForm : l.newBatch }}</button>
      </div>
      <p class="text-xs text-gray-500">{{ l.memoryHint }}</p>

      <form v-if="showForm" class="space-y-4 rounded-xl border bg-gray-50 p-4 dark:border-dark-600 dark:bg-dark-800" @submit.prevent="createBatch">
        <div>
          <label for="batch-backend" class="input-label">{{ t('admin.accounts.gemini.playground.sourceLabel') }}</label>
          <select id="batch-backend" v-model="backend" class="input" :disabled="busy">
            <option value="aistudio">{{ t('admin.accounts.gemini.playground.sourceAIStudio') }}</option>
            <option value="gemini_web">{{ t('admin.accounts.gemini.playground.sourceGeminiWeb') }}</option>
          </select>
        </div>
        <div class="flex items-center justify-between"><label for="batch-accounts" class="font-medium">{{ l.accounts }}</label><button type="button" class="text-sm text-primary-600" @click="showSecrets = !showSecrets">{{ showSecrets ? l.hideSecrets : l.showSecrets }}</button></div>
        <textarea id="batch-accounts" v-model="accountsText" rows="6" class="input font-mono text-sm" :class="{ 'secret-mask': !showSecrets }" autocomplete="off" autocorrect="off" spellcheck="false" :placeholder="l.accountFormat" />
        <p class="text-xs text-gray-500">{{ l.formatHint }} · {{ lineCount }} {{ l.rows }}</p>
        <div class="flex items-center justify-between"><label class="font-medium">{{ l.proxyPool }}</label><div class="flex gap-3"><button type="button" class="text-sm text-primary-600" @click="selectedProxies = proxies.map(p => p.id)">{{ l.selectAll }}</button><button type="button" class="text-sm text-primary-600" @click="showProxyImport = !showProxyImport">{{ l.importProxies }}</button></div></div>
        <div v-if="!proxies.length" class="rounded-lg bg-amber-50 p-3 text-sm text-amber-800 dark:bg-amber-900/20">{{ l.noProxies }}</div>
        <div class="grid max-h-40 grid-cols-1 gap-2 overflow-auto md:grid-cols-2">
          <label v-for="p in proxies" :key="p.id" class="flex gap-2 rounded border p-2 text-sm dark:border-dark-600"><input v-model="selectedProxies" type="checkbox" :value="p.id" /><span class="truncate">{{ proxyLabel(p) }}</span></label>
        </div>
        <p class="text-xs text-gray-500">{{ l.proxyHint }}</p>
        <div v-if="showProxyImport" class="space-y-2 rounded-lg border p-3 dark:border-dark-600">
          <select v-model="proxyProtocol" class="input w-36"><option value="http">HTTP</option><option value="https">HTTPS</option><option value="socks5">SOCKS5</option></select>
          <textarea v-model="proxyText" rows="3" class="input font-mono text-sm secret-mask" autocomplete="off" spellcheck="false" placeholder="IP:端口:用户名:密码" aria-label="代理导入内容" />
          <button type="button" class="btn btn-secondary" :disabled="busy || !proxyText.trim()" @click="importProxies">{{ l.importProxies }}</button>
        </div>
        <fieldset class="space-y-2 rounded-lg border p-3 dark:border-dark-600">
          <legend class="px-1 font-medium">{{ l.groups }}</legend>
          <p class="text-xs text-gray-500">{{ l.groupHint }}</p>
          <div v-if="geminiGroups.length" class="grid gap-2 md:grid-cols-2">
            <label v-for="g in geminiGroups" :key="g.id" class="flex items-center gap-2 rounded border p-2 text-sm dark:border-dark-600">
              <input v-model="selectedGroups" :data-group-id="g.id" type="checkbox" :value="g.id" :disabled="importUnassigned" @change="groupSelectionTouched = true" />{{ g.name }}
            </label>
          </div>
          <p v-else class="text-sm text-amber-700">{{ l.noGroups }}</p>
          <label class="flex items-start gap-2 text-sm"><input id="batch-unassigned" v-model="importUnassigned" type="checkbox" class="mt-1" @change="chooseUnassigned" /><span>{{ l.importUnassigned }}</span></label>
          <p v-if="!selectedGroups.length" role="status" class="text-sm text-amber-700">{{ l.unassignedWarning }}</p>
        </fieldset>
        <label class="flex items-start gap-2 text-sm"><input v-model="autoOnboarding" type="checkbox" class="mt-1" /><span>{{ l.onboarding }}</span></label>
        <div class="flex flex-wrap items-center gap-3"><button class="btn btn-primary" type="submit" :disabled="busy || !accountsText.trim() || !selectedProxies.length || (!selectedGroups.length && !importUnassigned)">{{ busy ? l.working : l.start }}</button><span class="text-xs text-gray-500">{{ l.secretHint }}</span></div>
      </form>

      <template v-if="state?.batches.length">
        <div class="flex flex-wrap gap-2">
          <select v-model="selectedBatch" class="input min-w-52 flex-1" aria-label="选择批次" @change="changeBatch"><option v-for="batch in state.batches" :key="batch.id" :value="batch.id">{{ batch.backend === 'gemini_web' ? 'Gemini Web' : 'AI Studio' }} · {{ new Date(batch.created).toLocaleString() }} · {{ Object.values(batch.counts).reduce((a,b) => a+b,0) }} {{ l.rows }} · {{ batch.counts.manual || 0 }} {{ l.manual }}</option></select>
          <button class="btn btn-secondary" :disabled="busy" @click="batchAction(currentBatch?.paused ? 'resume' : 'pause')">{{ currentBatch?.paused ? l.resume : l.pause }}</button>
          <button class="btn btn-secondary text-red-600" :disabled="busy" @click="batchAction('cancel')">{{ l.cancelBatch }}</button>
          <button v-if="canDelete" class="btn btn-secondary" :disabled="busy" @click="batchAction('delete')">{{ l.removeRecord }}</button>
        </div>
        <div class="flex flex-wrap gap-2" role="tablist" :aria-label="l.status">
          <button v-for="filter in filters" :key="filter" class="rounded-lg px-3 py-1.5 text-sm" :class="statusFilter === filter ? 'bg-primary-600 text-white' : 'bg-gray-100 dark:bg-dark-700'" role="tab" :aria-selected="statusFilter === filter" @click="statusFilter=filter; page=1">{{ statusName(filter) }} {{ filter === 'all' ? currentBatch?.jobs.length || 0 : currentBatch?.counts[filter] || 0 }}</button>
        </div>
        <div class="overflow-x-auto rounded-lg border dark:border-dark-600">
          <table class="w-full text-left text-sm"><thead class="bg-gray-50 text-xs text-gray-500 dark:bg-dark-800"><tr><th class="p-3">{{ l.email }}</th><th class="p-3">{{ l.status }}</th><th class="p-3">{{ l.progress }}</th><th class="p-3">{{ l.proxy }}</th><th class="p-3">{{ l.actions }}</th></tr></thead>
            <tbody><tr v-for="job in visibleJobs" :key="job.id" class="border-t dark:border-dark-600"><td class="p-3">{{ job.email }}</td><td class="p-3"><span :class="job.status === 'manual' ? 'text-amber-600' : job.status === 'failed' ? 'text-red-600' : job.status === 'succeeded' ? 'text-green-600' : ''">{{ statusName(job.status) }}</span></td><td class="max-w-xs p-3"><div>{{ stepName(job.step) }}</div><div v-if="job.reason" class="mt-1 text-xs text-gray-500">{{ job.reason }}</div></td><td class="p-3 text-xs">{{ job.proxy_name || '—' }}</td><td class="p-3"><div class="flex flex-wrap gap-2"><button v-if="['manual','failed'].includes(job.status) && job.proxy_id" class="text-primary-600" @click="openManual(job)">{{ l.handle }}</button><button v-if="['manual','failed'].includes(job.status) && job.has_credentials && !job.manual_open && !job.opening" class="text-primary-600" @click="jobAction(job, 'retry')">{{ l.retry }}</button><button v-if="['queued','running','manual','failed'].includes(job.status)" class="text-red-600" @click="jobAction(job, 'cancel')">{{ l.cancel }}</button><span v-if="job.account_id" class="text-xs text-gray-500">#{{ job.account_id }}</span></div></td></tr></tbody>
          </table>
          <p v-if="!filteredJobs.length" class="p-6 text-center text-gray-500">{{ l.empty }}</p>
        </div>
        <div v-if="filteredJobs.length>50" class="flex items-center justify-end gap-3"><button class="btn btn-secondary" :disabled="page<=1" @click="page--">{{ l.previous }}</button><span>{{ page }} / {{ Math.ceil(filteredJobs.length/50) }}</span><button class="btn btn-secondary" :disabled="page*50>=filteredJobs.length" @click="page++">{{ l.next }}</button></div>
      </template>

      <section v-if="manualId" class="space-y-3 rounded-xl border border-amber-300 p-4 dark:border-amber-700">
        <div class="flex items-center justify-between"><h4 class="font-semibold">{{ l.manual }} · {{ manualJob?.email }}</h4><button class="btn btn-secondary" @click="closeManual">{{ l.closeWindow }}</button></div>
        <p class="text-sm text-gray-600 dark:text-gray-300">{{ l.manualHint }}</p>
        <p v-if="!frameLive" class="text-sm text-amber-700">{{ l.waitingWindow }}</p>
        <div tabindex="0" class="overflow-hidden rounded-lg border focus:outline-none focus:ring-2 focus:ring-primary-500" role="group" :aria-label="l.manual" @keydown.stop="keyInput" @paste.prevent="pasteInput" @wheel.prevent="scrollInput">
          <img v-if="frame" :src="`data:image/jpeg;base64,${frame}`" :alt="l.manual" class="w-full" draggable="false" @click="clickInput" />
          <p v-else class="p-8 text-center text-gray-500">{{ l.working }}</p>
        </div>
        <div class="flex flex-wrap gap-2"><button class="btn btn-primary" :disabled="busy || !frameLive" @click="finishManual">{{ l.finish }}</button><button class="btn btn-secondary" :disabled="busy || !frameLive || !manualJob?.has_credentials" @click="continueAuto">{{ l.continueAuto }}</button><button v-if="!manualJob?.manual_open && !manualJob?.opening" class="btn btn-secondary" @click="reopenManual">{{ l.reopen }}</button></div>
      </section>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { playgroundBatchAPI, parseBatchProxyText, type LoginBatchState, type LoginJob } from '@/api/admin/playgroundBatch'
import proxiesAPI from '@/api/admin/proxies'
import type { PlaygroundBrowserInput, PlaygroundBackend } from '@/api/admin/accounts'
import type { Group, Proxy } from '@/types'

const props = defineProps<{ show: boolean; groups: Group[] }>()
const emit = defineEmits<{ close: []; imported: [] }>()
const { locale, t } = useI18n()
const cn = {title:'Playground 批量上号',intro:'批量登录 Google 并自动保存到账号池。人机验证和额外安全验证会转入人工处理。',running:'正在运行的上号浏览器',resourceSlots:'当前浏览器资源上限',manual:'待人工处理',saved:'已成功保存',concurrency:'全局上号并发',apply:'应用并发',hideForm:'收起导入',newBatch:'新建批次',memoryHint:'并发支持 10、20 等目标值。可用内存不足时自动排队；正在执行的业务会话优先。',accounts:'批量账号',showSecrets:'显示输入',hideSecrets:'隐藏输入',accountFormat:'user@example.com----密码----2FA密钥',formatHint:'每行一个账号，2FA 密钥可带空格；未开启 2FA 可留空',rows:'个账号',proxyPool:'上号代理池',selectAll:'全选',importProxies:'批量导入代理',noProxies:'请先导入有效代理。批量上号不会使用服务器直连。',proxyHint:'每个代理同时处理一个上号任务，本次登录保持同一代理；保存账号后继续使用该代理。',groups:'保存到可调用分组',groupHint:'账号加入分组后才会进入该分组的可用号池。使用 OpenAI 协议时，也请选择 Gemini 分组并为该分组创建 API Key。',noGroups:'还没有可用的 Gemini 分组，请先在分组管理中创建；也可明确选择暂不分组。',importUnassigned:'仅导入账号，暂不加入可用号池',unassignedWarning:'未分组的账号不会被 API 请求选中；稍后可在账户页批量设置分组。',selectGroup:'请选择保存分组，或勾选仅导入账号。',onboarding:'自动完成已支持的 AI Studio 首次引导，未知引导页面转人工处理。',start:'导入并开始',working:'处理中…',secretHint:'上号密码和 2FA 密钥加密保存，成功或取消后清除；未完成任务最多保留 24 小时。',pause:'暂停新任务',resume:'继续队列',cancelBatch:'取消未完成任务',removeRecord:'清理批次记录',status:'状态',email:'邮箱',progress:'当前步骤',proxy:'代理出口',actions:'操作',handle:'人工处理',retry:'重试',cancel:'取消',empty:'当前没有这类任务',previous:'上一页',next:'下一页',closeWindow:'关闭人工窗口',manualHint:'点击画面后可以输入、粘贴、滚动。完成验证码后可继续自动执行；进入 AI Studio 后可直接保存。',waitingWindow:'正在等待人工窗口。此前截图仅供参考，窗口就绪后可操作。',finish:'完成并保存账号',continueAuto:'继续自动执行',reopen:'重新打开',proxyImported:'代理已导入',started:'批次已创建，自动执行中',done:'账号已保存',updated:'并发设置已保存'}
const en = {title:'Playground batch sign-in',intro:'Sign in to Google accounts and save them to the pool. CAPTCHA and additional verification are handed to you.',running:'Active sign-in browsers',resourceSlots:'Available browser capacity',manual:'Needs attention',saved:'Saved accounts',concurrency:'Global sign-in concurrency',apply:'Apply',hideForm:'Hide import',newBatch:'New batch',memoryHint:'Targets such as 10 or 20 are supported. Tasks queue when memory is limited; existing business sessions have priority.',accounts:'Accounts',showSecrets:'Show input',hideSecrets:'Hide input',accountFormat:'user@example.com----password----TOTP secret',formatHint:'One account per line. Spaces in TOTP secrets are accepted; leave the last field empty without TOTP.',rows:'accounts',proxyPool:'Sign-in proxy pool',selectAll:'Select all',importProxies:'Import proxies',noProxies:'Import an active proxy first. Batch sign-in never falls back to a direct server connection.',proxyHint:'One active sign-in per proxy. Each login keeps its proxy, which is also assigned to the saved account.',groups:'Save to callable groups',groupHint:'Accounts must belong to a group to serve requests. For OpenAI-compatible clients, use a Gemini group and an API key bound to that group.',noGroups:'Create a Gemini group first, or explicitly import without assigning a group.',importUnassigned:'Import accounts without making them available to a group',unassignedWarning:'Ungrouped accounts cannot serve API requests. Assign groups later from the Accounts page.',selectGroup:'Select a group or explicitly choose to import unassigned accounts.',onboarding:'Complete supported AI Studio onboarding automatically; hand unfamiliar pages to the operator.',start:'Import and start',working:'Working…',secretHint:'Credentials are encrypted and cleared after success or cancellation. Unfinished credentials expire after 24 hours.',pause:'Pause new tasks',resume:'Resume queue',cancelBatch:'Cancel unfinished tasks',removeRecord:'Remove batch record',status:'Status',email:'Email',progress:'Current step',proxy:'Proxy',actions:'Actions',handle:'Take over',retry:'Retry',cancel:'Cancel',empty:'No matching tasks',previous:'Previous',next:'Next',closeWindow:'Close browser',manualHint:'Click to type, paste or scroll. Resume automation after verification, or save after reaching the selected website.',waitingWindow:'Waiting for the live browser. An earlier screenshot may be displayed until it is ready.',finish:'Finish and save account',continueAuto:'Resume automation',reopen:'Open again',proxyImported:'Proxies imported',started:'Batch created',done:'Account saved',updated:'Concurrency saved'}
const l = computed(() => locale.value.startsWith('zh') ? cn : en)
const state = ref<LoginBatchState>(); const selectedBatch = ref(''); const selectedProxies = ref<number[]>([]); const selectedGroups = ref<number[]>([])
const importUnassigned = ref(false); const groupSelectionTouched = ref(false)
const backend = ref<PlaygroundBackend>('aistudio')
const proxies = ref<Proxy[]>([]); const accountsText = ref(''); const proxyText = ref(''); const proxyProtocol = ref('http')
const concurrency = ref(2); const autoOnboarding = ref(true); const showForm = ref(true); const showProxyImport = ref(false); const showSecrets = ref(false)
const busy = ref(false); const error = ref(''); const notice = ref(''); const statusFilter = ref('all'); const page = ref(1)
const manualId = ref(''); const manualBatchId = ref(''); const frame = ref(''); const frameLive = ref(false)
let pollTimer: ReturnType<typeof setTimeout> | undefined; let frameTimer: ReturnType<typeof setTimeout> | undefined
let generation = 0; let inputs: Promise<void> = Promise.resolve(); let successCount = 0
const filters = ['all','manual','running','queued','failed','succeeded','skipped','cancelled']
const currentBatch = computed(() => state.value?.batches.find(b => b.id === selectedBatch.value))
const manualJob = computed(() => currentBatch.value?.jobs.find(j => j.id === manualId.value))
const filteredJobs = computed(() => (currentBatch.value?.jobs || []).filter(j => statusFilter.value === 'all' || j.status === statusFilter.value))
const visibleJobs = computed(() => filteredJobs.value.slice((page.value - 1) * 50, page.value * 50))
const canDelete = computed(() => !['queued','running','manual'].some(k => (currentBatch.value?.counts[k] || 0) > 0))
const geminiGroups = computed(() => props.groups.filter(g => ['gemini', 'composite'].includes(g.platform) && g.status === 'active'))
const lineCount = computed(() => accountsText.value.split(/\r?\n/).filter(line => line.trim()).length)
function proxyLabel(p: Proxy) { return `${p.protocol}://${p.host}:${p.port}` }
function totalCount(kind: string) { return state.value?.batches.reduce((n,b) => n + (b.counts[kind] || 0), 0) || 0 }
function statusName(key: string) { const values: Record<string,string> = locale.value.startsWith('zh') ? {all:'全部',queued:'排队',running:'处理中',manual:'待人工',failed:'失败',succeeded:'成功',skipped:'已跳过',cancelled:'已取消'} : {all:'All',queued:'Queued',running:'Running',manual:'Manual',failed:'Failed',succeeded:'Saved',skipped:'Skipped',cancelled:'Cancelled'}; return values[key] || key }
function stepName(key: string) { const values: Record<string,string> = {checking_proxy:'检查代理连接',opening_browser:'打开登录浏览器',browser_ready:'读取登录页面',email:'提交邮箱',password:'提交密码',totp:'提交动态验证码',saving_account:'保存账号',manual_opening:'等待人工窗口',resuming:'继续自动登录',choose_account:'选择账号',other_account:'切换登录账号',choose_totp:'选择身份验证器',optional_skip:'跳过可选步骤',onboarding_check:'处理首次引导',onboarding_continue:'提交首次引导'}; return locale.value.startsWith('zh') ? values[key] || statusName(key) : key.replace(/_/g,' ') }
function failure(cause: unknown) { const e = cause as { response?: { data?: { message?: string; detail?: string } }; message?: string }; error.value = e.response?.data?.message || e.response?.data?.detail || e.message || 'Request failed' }
async function refreshProxies() { const previous = new Set(proxies.value.map(p => p.id)); proxies.value = (await proxiesAPI.getAll()).filter(p => { const expires = p.expires_at ? typeof p.expires_at === 'number' ? p.expires_at * 1000 : Date.parse(p.expires_at) : 0; return p.status === 'active' && (!expires || expires > Date.now()) }); selectedProxies.value = proxies.value.filter(p => selectedProxies.value.includes(p.id) || !previous.has(p.id)).map(p => p.id) }
let loadSequence = 0
async function load(ticket = generation) {
  const request = ++loadSequence
  const selection = selectedBatch.value
  const value = await playgroundBatchAPI.list(selection || undefined)
  if (!props.show || ticket !== generation || request !== loadSequence || selection !== selectedBatch.value) return
  state.value = value
  if (!value.batches.some(b => b.id === selection)) {
    const next = value.batches[0]?.id || ''
    selectedBatch.value = next
    if (next && !value.batches[0]?.jobs.length) {
      const detail = await playgroundBatchAPI.list(next)
      if (!props.show || ticket !== generation || request !== loadSequence || selectedBatch.value !== next) return
      state.value = detail
    }
  }
  const count = totalCount('succeeded')
  if (count > successCount) emit('imported')
  successCount = count
  const selected = currentBatch.value?.jobs.find(j => j.id === manualId.value)
  if (selected && ['succeeded','skipped','cancelled'].includes(selected.status)) void closeManual()
}
async function poll(ticket: number) { if (!props.show || ticket !== generation) return; try { await load(ticket) } catch(cause) { if (props.show && ticket === generation) failure(cause) } if (props.show && ticket === generation) pollTimer = setTimeout(() => poll(ticket), 2000) }
async function createBatch() { if (!selectedGroups.value.length && !importUnassigned.value) { error.value = l.value.selectGroup; return } busy.value = true; error.value = ''; notice.value = ''; try { const result = await playgroundBatchAPI.create({accounts_text:accountsText.value,proxy_ids:selectedProxies.value,group_ids:importUnassigned.value ? [] : selectedGroups.value,concurrency:concurrency.value,auto_onboarding:autoOnboarding.value,backend:backend.value}); accountsText.value = ''; selectedBatch.value = result.id; showForm.value = false; statusFilter.value = 'all'; page.value = 1; notice.value = l.value.started; await load() } catch(cause) { failure(cause) } finally { busy.value = false } }
async function importProxies() { busy.value = true; error.value = ''; try { const rows = parseBatchProxyText(proxyText.value,proxyProtocol.value); const result = await proxiesAPI.batchCreate(rows); proxyText.value = ''; await refreshProxies(); notice.value = `${l.value.proxyImported}: ${result.created} / ${result.skipped}` } catch(cause) { failure(cause) } finally { busy.value = false } }
async function applyConcurrency() { busy.value = true; try { await playgroundBatchAPI.configure(concurrency.value); notice.value = l.value.updated; await load() } catch(cause) { failure(cause) } finally { busy.value = false } }
async function batchAction(action: string) { if (!selectedBatch.value) return; busy.value = true; try { await playgroundBatchAPI.action(selectedBatch.value,action); await load() } catch(cause) { failure(cause) } finally { busy.value = false } }
async function jobAction(job: LoginJob, action: string) { try { await playgroundBatchAPI.action(selectedBatch.value,action,job.id); await load() } catch(cause) { failure(cause) } }
async function changeBatch() { await closeManual(); page.value = 1; await load() }
async function pollFrame(id: string, batch: string) {
  const ticket = generation
  const current = () => props.show && ticket === generation && manualId.value === id && manualBatchId.value === batch && selectedBatch.value === batch
  if (!current()) return
  try {
    if (!busy.value) {
      const result = await playgroundBatchAPI.frame(batch,id)
      if (current()) { frame.value = result.image; frameLive.value = result.live }
    }
  } catch(cause) {
    const e = cause as { response?: { data?: { message?: string } }; message?: string }
    const message = e.response?.data?.message || e.message || ''
    if (current() && !message.includes('正忙')) frameLive.value = false
  }
  if (current()) frameTimer = setTimeout(() => pollFrame(id,batch),1200)
}
async function openManual(job: LoginJob) { if (manualId.value === job.id) return; await closeManual(); manualId.value = job.id; manualBatchId.value = selectedBatch.value; frame.value = ''; frameLive.value = false; error.value = ''; try { if (!job.manual_open && !job.opening) await playgroundBatchAPI.action(manualBatchId.value,'open_manual',job.id); await load(); void pollFrame(job.id,manualBatchId.value) } catch(cause) { failure(cause) } }
async function reopenManual() { const job = manualJob.value; if (job) { await closeManual(); await openManual(job) } }
async function closeManual() { clearTimeout(frameTimer); const id = manualId.value; const batch = manualBatchId.value; manualId.value = ''; manualBatchId.value = ''; frame.value = ''; frameLive.value = false; if (id && batch) { try { await playgroundBatchAPI.action(batch,'close_manual',id) } catch { /* Server expires abandoned windows. */ } } }
function send(input: PlaygroundBrowserInput) { const id = manualId.value; const batch = manualBatchId.value; if (!id || !frameLive.value || busy.value) return; inputs = inputs.then(async () => { if (manualId.value === id) await playgroundBatchAPI.input(batch,id,input) }).catch(failure) }
function clickInput(event: MouseEvent) { const image = event.target as HTMLImageElement; const rect = image.getBoundingClientRect(); image.parentElement?.focus(); send({type:'click',x:Math.round((event.clientX-rect.left)*1100/rect.width),y:Math.round((event.clientY-rect.top)*780/rect.height)}) }
function keyInput(event: KeyboardEvent) { if (event.ctrlKey || event.metaKey || event.altKey) return; if (event.key === 'Escape') { (event.currentTarget as HTMLElement).blur(); return } if (['Enter','Tab','Backspace','ArrowDown','ArrowUp'].includes(event.key)) { event.preventDefault(); send({type:'key',key:event.key}) } else if (event.key.length === 1) { event.preventDefault(); send({type:'text',text:event.key}) } }
function pasteInput(event: ClipboardEvent) { const text=event.clipboardData?.getData('text'); if(text) send({type:'text',text}) }
function scrollInput(event: WheelEvent) { send({type:'scroll',delta_y:Math.max(-1000,Math.min(1000,Math.round(event.deltaY)))}) }
async function finishManual() { if (!manualId.value) return; busy.value = true; error.value=''; try { await inputs; await playgroundBatchAPI.finish(selectedBatch.value,manualId.value); notice.value=l.value.done; await closeManual(); await load(); emit('imported') } catch(cause) { failure(cause) } finally { busy.value=false } }
async function continueAuto() { if(!manualId.value)return; busy.value=true; try { await inputs; await playgroundBatchAPI.action(selectedBatch.value,'continue_auto',manualId.value); frameLive.value=false; await load() } catch(cause) { failure(cause) } finally { busy.value=false } }
async function close() { generation++; clearTimeout(pollTimer); await closeManual(); accountsText.value=''; proxyText.value=''; emit('close') }
function chooseUnassigned() { groupSelectionTouched.value = true; if (importUnassigned.value) selectedGroups.value = [] }
watch([() => props.show, geminiGroups], ([show], old) => {
  if (!show) return
  if (!old?.[0]) { groupSelectionTouched.value = false; importUnassigned.value = false }
  const valid = new Set(geminiGroups.value.map(g => g.id))
  selectedGroups.value = selectedGroups.value.filter(id => valid.has(id))
  if (!groupSelectionTouched.value && !selectedGroups.value.length && geminiGroups.value.length === 1) selectedGroups.value = [geminiGroups.value[0].id]
}, { immediate: true })
watch(() => props.show, async show => { generation++; clearTimeout(pollTimer); if(!show) { accountsText.value=''; proxyText.value=''; await closeManual(); return } const ticket=generation; error.value=''; try { await refreshProxies(); await load(); concurrency.value=state.value?.concurrency || 2; showForm.value=!state.value?.batches.length; void poll(ticket) } catch(cause) { failure(cause) } }, { immediate: true })
onBeforeUnmount(() => { generation++; clearTimeout(pollTimer); clearTimeout(frameTimer); void closeManual(); accountsText.value=''; proxyText.value='' })
</script>

<style scoped>
.secret-mask { -webkit-text-security: disc; }
</style>
