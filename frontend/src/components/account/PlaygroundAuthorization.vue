<template>
  <section class="space-y-4" :aria-label="t(`${prefix}.authTitle`)">
    <div class="rounded-lg border border-gray-200 p-4 dark:border-dark-600">
      <h4 class="font-medium">{{ t(`${prefix}.authTitle`) }}</h4>
      <p class="mt-2 text-sm text-gray-600 dark:text-dark-300">{{ t(`${prefix}.${configured ? 'authConfigured' : 'authNotConfigured'}`) }}</p>
      <p v-if="email" class="mt-1 text-sm">{{ email }}</p>
      <button v-if="!loginId" type="button" class="btn btn-secondary mt-3" :disabled="busy" @click="start">
        {{ t(`${prefix}.${busy ? 'browserStarting' : editing || loginToken ? 'authChange' : 'browserLogin'}`) }}
      </button>
      <button v-if="busy && !loginId" type="button" class="btn btn-secondary ml-2 mt-3" @click="cancel()">{{ t('common.cancel') }}</button>
    </div>
    <template v-if="loginId">
      <p :id="helpId" class="text-sm text-gray-600 dark:text-dark-300">{{ t(`${prefix}.browserHelp`) }}</p>
      <div tabindex="0" role="group" :aria-label="t(`${prefix}.browserView`)" :aria-describedby="helpId"
        class="overflow-hidden rounded-lg border border-gray-300 focus:outline-none focus:ring-2 focus:ring-primary-500 dark:border-dark-600"
        @keydown="onKey" @paste.prevent="onPaste" @wheel.prevent="onWheel">
        <img v-if="frame" :src="`data:image/jpeg;base64,${frame}`" :alt="t(`${prefix}.browserView`)" class="w-full" draggable="false" @click="onClick" />
        <p v-else role="status" class="p-6 text-sm">{{ t(`${prefix}.browserStarting`) }}</p>
      </div>
      <div class="flex flex-wrap gap-2">
        <button type="button" class="btn btn-primary" :disabled="busy" @click="finish">{{ t(`${prefix}.browserFinish`) }}</button>
        <button type="button" class="btn btn-secondary" @click="cancel()">{{ t('common.cancel') }}</button>
      </div>
    </template>
    <p v-if="loginToken" role="status" class="text-sm text-green-700 dark:text-green-400">{{ t(`${prefix}.browserReady`) }}</p>
    <p v-if="error" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ error }}</p>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, useId, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { startPlaygroundLogin, playgroundLoginFrame, playgroundLoginInput, finishPlaygroundLogin, cancelPlaygroundLogin, type PlaygroundBrowserInput } from '@/api/admin/accounts'

const props = defineProps<{ editing?: boolean; source?: unknown; proxyId?: number | null }>()
const loginToken = defineModel<string>('loginToken', { default: '' })
const { t } = useI18n()
const helpId = useId()
const prefix = 'admin.accounts.gemini.playground'
const loginId = ref('')
const frame = ref('')
const error = ref('')
const busy = ref(false)
const capturedEmail = ref('')
const email = computed(() => capturedEmail.value || (props.source as { email?: string } | undefined)?.email || '')
const configured = computed(() => !!loginToken.value || (props.source as { browser?: string } | undefined)?.browser === 'camoufox')
let timer: ReturnType<typeof setTimeout> | undefined
let controller: AbortController | undefined
let disposed = false
let generation = 0
let inputs: Promise<void> = Promise.resolve()

function failure(cause: unknown) {
  const e = cause as { response?: { data?: { detail?: string; message?: string } }; message?: string }
  error.value = e.response?.data?.detail || e.response?.data?.message || e.message || t(`${prefix}.browserFailed`)
}
async function poll() {
  const id = loginId.value
  if (!id || disposed) return
  try {
    if (!busy.value) {
      const result = await playgroundLoginFrame(id)
      if (loginId.value === id) frame.value = result.image
    }
  } catch (cause) { if (loginId.value === id) { failure(cause); await cancel(false); return } }
  if (loginId.value === id && !disposed) timer = setTimeout(poll, 1200)
}
async function start() {
  const ticket = ++generation
  error.value = ''; busy.value = true; loginToken.value = ''
  const ownController = new AbortController(); controller = ownController
  try {
    const result = await startPlaygroundLogin(props.proxyId, ownController.signal)
    if (disposed || ownController.signal.aborted) { await cancelPlaygroundLogin(result.id); return }
    loginId.value = result.id
  } catch (cause) { if (!ownController.signal.aborted) failure(cause) }
  finally { if (ticket === generation) busy.value = false }
  if (ticket === generation && loginId.value) void poll()
}
async function cancel(clearError = true) {
  generation++
  controller?.abort(); clearTimeout(timer)
  const id = loginId.value; loginId.value = ''; frame.value = ''; busy.value = false
  if (clearError) error.value = ''
  if (id) { try { await cancelPlaygroundLogin(id) } catch { /* Server expires abandoned login jobs. */ } }
}
function send(input: PlaygroundBrowserInput) {
  const id = loginId.value
  if (!id || busy.value) return
  inputs = inputs.then(async () => { if (id === loginId.value) await playgroundLoginInput(id, input) }).catch(failure)
}
function onClick(event: MouseEvent) {
  const image = event.target as HTMLImageElement
  const rect = image.getBoundingClientRect()
  image.parentElement?.focus()
  send({ type: 'click', x: Math.round((event.clientX - rect.left) * 1100 / rect.width), y: Math.round((event.clientY - rect.top) * 780 / rect.height) })
}
function onKey(event: KeyboardEvent) {
  if (event.ctrlKey || event.metaKey || event.altKey) return
  if (['Enter', 'Tab', 'Backspace', 'Escape', 'ArrowDown', 'ArrowUp'].includes(event.key)) {
    if (event.key === 'Escape') { (event.currentTarget as HTMLElement).blur(); return }
    event.preventDefault(); send({ type: 'key', key: event.key })
  } else if (event.key.length === 1) { event.preventDefault(); send({ type: 'text', text: event.key }) }
}
function onPaste(event: ClipboardEvent) { const text = event.clipboardData?.getData('text'); if (text) send({ type: 'text', text }) }
function onWheel(event: WheelEvent) { send({ type: 'scroll', delta_y: Math.max(-1000, Math.min(1000, Math.round(event.deltaY))) }) }
async function finish() {
  const ticket = generation
  busy.value = true; error.value = ''
  const id = loginId.value
  try {
    await inputs
    const result = await finishPlaygroundLogin(id)
    if (id !== loginId.value || disposed) return
    loginToken.value = result.token; capturedEmail.value = result.email
    clearTimeout(timer); loginId.value = ''; frame.value = ''
  } catch (cause) { failure(cause) }
  finally { if (ticket === generation) busy.value = false }
}
watch(() => props.proxyId, () => { loginToken.value = ''; capturedEmail.value = ''; void cancel() })
onBeforeUnmount(() => { disposed = true; void cancel() })
</script>
