<template>
  <button class="btn btn-secondary" @click="open">{{ l.title }}</button>
  <BaseDialog :show="show" :title="l.title" width="wide" @close="close">
    <div class="space-y-4">
      <p class="text-sm text-gray-600 dark:text-gray-300">{{ l.intro }}</p>
      <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
      <p v-if="notice" role="status" class="text-sm text-green-600">{{ notice }}</p>
      <template v-if="state">
        <div class="grid grid-cols-2 gap-2 text-sm md:grid-cols-4" aria-live="polite">
          <div class="rounded border p-2 dark:border-dark-600">{{ l.ready }} <strong>{{ state.ready }}</strong> / {{ state.eligible }}</div>
          <div class="rounded border p-2 dark:border-dark-600">{{ l.cached }} <strong>{{ state.cached }} / {{ state.light_cached }}</strong></div>
          <div class="rounded border p-2 dark:border-dark-600">{{ l.active }} <strong>{{ state.active_requests }}</strong></div>
          <div class="rounded border p-2 dark:border-dark-600">{{ l.memory }} <strong>{{ state.memory_headroom_mb < 0 ? '—' : `${state.memory_headroom_mb} MiB` }}</strong></div>
        </div>
        <p class="text-sm" role="status">{{ state.warming_account_id ? `${l.warming} #${state.warming_account_id}` : pauseLabel(state.paused_reason) }}</p>
        <form class="space-y-3" @submit.prevent="save">
          <label class="flex items-center gap-2 text-sm"><input v-model="form.enabled" type="checkbox" data-test="warmup-enabled" />{{ l.enabled }}</label>
          <div class="grid grid-cols-1 gap-3 sm:grid-cols-3">
            <label class="text-sm">{{ l.target }}<input v-model.number="form.target" class="input mt-1" type="number" min="0" :max="form.max_cached" data-test="warmup-target" required /></label>
            <label class="text-sm">{{ l.limit }}<input v-model.number="form.max_cached" class="input mt-1" type="number" min="1" max="1000" data-test="warmup-cache" required /></label>
            <label class="text-sm">{{ l.idle }}<input v-model.number="form.idle_minutes" class="input mt-1" type="number" min="5" max="240" data-test="warmup-idle" required /></label>
          </div>
          <p class="text-xs text-gray-500">{{ l.hint }}</p>
          <button class="btn btn-primary" :disabled="saving || !valid">{{ saving ? l.saving : l.save }}</button>
        </form>
        <div v-if="state.accounts.length" class="max-h-72 overflow-auto rounded border dark:border-dark-600">
          <table class="w-full text-left text-sm">
            <thead class="bg-gray-50 dark:bg-dark-800"><tr><th class="p-2">{{ l.account }}</th><th class="p-2">{{ l.status }}</th><th class="p-2">{{ l.duration }}</th><th class="p-2">{{ l.requestPreparation }}</th><th class="p-2">{{ l.lastReady }}</th></tr></thead>
            <tbody><tr v-for="account in state.accounts" :key="account.account_id" class="border-t dark:border-dark-600"><td class="p-2">#{{ account.account_id }}</td><td class="p-2">{{ statusLabel(account.state) }}<p v-if="account.error" class="max-w-xs text-xs text-amber-700">{{ account.error }}</p></td><td class="p-2">{{ (account.duration_ms / 1000).toFixed(2) }}s</td><td class="p-2">{{ account.request_preparation_ms == null ? '—' : `${(account.request_preparation_ms / 1000).toFixed(2)}s` }}</td><td class="p-2">{{ dateLabel(account.prepared_at) }}</td></tr></tbody>
          </table>
        </div>
      </template>
      <p v-else-if="!error" class="text-sm">{{ l.loading }}</p>
    </div>
  </BaseDialog>
</template>
<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { playgroundWarmupAPI, type PlaygroundWarmupConfig, type PlaygroundWarmupState } from '@/api/admin/playgroundWarmup'

const { locale } = useI18n()
const zh = computed(() => locale?.value?.startsWith('zh') ?? true)
const cn = { title:'Playground 预热', intro:'提前准备登录态、页面和请求校验环境，不发送生成请求。浏览器在恢复登录态后关闭，内存不足时暂停预热，并为同账号请求和业务登录态恢复让路。', ready:'就绪 / 可用', cached:'热会话 / 轻缓存', active:'业务请求', memory:'内存余量', warming:'正在预热账号', enabled:'启用自动预热', target:'提前预热的账号数', limit:'会话缓存上限', idle:'闲置回收（分钟）', hint:'预热数量不是请求并发上限。按原轮询顺序提前准备账号；超出缓存上限时释放运行环境、保留轻量页面信息，闲置后完全回收；正在响应的请求不会因回收被中断。超过预热范围的账号首次使用仍可能需要准备。', save:'保存并应用', saving:'保存中…', saved:'已保存，后台将按新设置预热', loading:'正在读取状态…', account:'账号', status:'状态', duration:'初始化耗时', requestPreparation:'最近请求准备', lastReady:'最近就绪（北京时间）', loadError:'读取预热状态失败', saveError:'保存预热设置失败' }
const en: typeof cn = { title:'Playground warmup', intro:'Prepare login state, page and request validation without generating content. Recovery browsers close immediately. Warmup yields to the same account, foreground login recovery and memory pressure.', ready:'Ready / eligible', cached:'Hot / lightweight', active:'Active requests', memory:'Memory headroom', warming:'Warming account', enabled:'Enable automatic warmup', target:'Accounts to prewarm', limit:'Session cache limit', idle:'Idle eviction (minutes)', hint:'The warmup target is not a request concurrency limit. Accounts are prepared in the existing rotation order; eviction retains lightweight page state until idle expiry without interrupting active responses. Accounts outside the hot set may still need preparation.', save:'Save and apply', saving:'Saving…', saved:'Saved; background warmup will apply these settings', loading:'Loading status…', account:'Account', status:'Status', duration:'Initialization', requestPreparation:'Last request prep', lastReady:'Last ready (UTC+8)', loadError:'Could not load warmup status', saveError:'Could not save warmup settings' }
const l = computed(() => zh.value ? cn : en)
const show = ref(false)
const state = ref<PlaygroundWarmupState | null>(null)
const form = ref<PlaygroundWarmupConfig>({ enabled:true, target:8, max_cached:16, idle_minutes:30 })
const error = ref('')
const notice = ref('')
const saving = ref(false)
let generation = 0
let timer: ReturnType<typeof setTimeout> | undefined
let controller: AbortController | undefined
const valid = computed(() => [form.value.target, form.value.max_cached, form.value.idle_minutes].every(Number.isInteger) && form.value.target >= 0 && form.value.target <= form.value.max_cached && form.value.max_cached >= 1 && form.value.max_cached <= 1000 && form.value.idle_minutes >= 5 && form.value.idle_minutes <= 240)
function pauseLabel(reason?: string) {
  const labels: Record<string,string> = zh.value ? { disabled:'自动预热已关闭', foreground:'业务请求优先，预热暂候', memory:'内存余量不足，预热暂候', settings_unavailable:'配置服务暂不可用，预热暂停' } : { disabled:'Automatic warmup is disabled', foreground:'Yielding to foreground requests', memory:'Waiting for memory headroom', settings_unavailable:'Settings unavailable; warmup paused' }
  return reason ? labels[reason] || reason : zh.value ? '后台自动维护就绪账号' : 'Maintaining ready accounts in the background'
}
function statusLabel(status: string) {
  const labels: Record<string,string> = zh.value ? { ready:'已就绪', busy:'使用中', cold:'待准备', warming:'预热中', deferred:'稍后继续', failed:'准备失败', evicted:'已回收', standby:'页面已准备' } : { ready:'Ready', busy:'In use', cold:'Not ready', warming:'Warming', deferred:'Deferred', failed:'Failed', evicted:'Evicted', standby:'Page ready' }
  return labels[status] || status
}
function dateLabel(value?: string) { return value ? new Date(value).toLocaleString(zh.value ? 'zh-CN' : 'en-GB', { timeZone:'Asia/Shanghai' }) : '—' }
async function load(initial: boolean, current: number) {
  controller = new AbortController()
  try {
    const value = await playgroundWarmupAPI.get(controller.signal)
    if (!show.value || current !== generation) return
    state.value = value
    if (initial) form.value = { ...value.config }
    error.value = ''
  } catch {
    if (show.value && current === generation) error.value = l.value.loadError
  } finally {
    if (show.value && current === generation) timer = setTimeout(() => { void load(false, current) }, 5000)
  }
}
function open() { close(); show.value = true; state.value = null; error.value = ''; notice.value = ''; void load(true, generation) }
function close() { show.value = false; generation++; if (timer) clearTimeout(timer); controller?.abort() }
async function save() {
  if (!valid.value || saving.value) return
  const current = generation
  saving.value = true; error.value = ''; notice.value = ''
  try {
    const result = await playgroundWarmupAPI.configure({ ...form.value })
    if (show.value && current === generation) { state.value = result; notice.value = l.value.saved }
  } catch { if (current === generation) error.value = l.value.saveError }
  finally { saving.value = false }
}
onBeforeUnmount(close)
</script>
