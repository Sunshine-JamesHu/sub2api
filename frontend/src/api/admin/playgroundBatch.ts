import { apiClient } from '../client'
import type { PlaygroundBrowserInput, PlaygroundBackend } from './accounts'

export interface LoginJob {
  id: string; email: string; status: string; step: string; reason?: string
  proxy_id: number; proxy_name: string; attempts: number; account_id?: number
  has_credentials: boolean; manual_open: boolean; opening: boolean; updated: string
}
export interface LoginBatch {
  backend?: PlaygroundBackend
  id: string; paused: boolean; created: string; proxy_ids: number[]; group_ids: number[]
  auto_onboarding: boolean; counts: Record<string, number>; jobs: LoginJob[]
}
export interface LoginBatchState {
  concurrency: number; running: number; error?: string
  capacity: { configured: number; effective: number; active: number; waiting: number; reason?: string }
  batches: LoginBatch[]
}
const base = '/admin/accounts/playground/batches'
export const playgroundBatchAPI = {
  async list(batchId?: string): Promise<LoginBatchState> {
    return (await apiClient.get<LoginBatchState>(base, { params: { batch_id: batchId }, headers: { 'Cache-Control': 'no-cache' } })).data
  },
  async create(body: { accounts_text: string; proxy_ids: number[]; group_ids: number[]; concurrency: number; auto_onboarding: boolean; backend?: PlaygroundBackend }): Promise<{ id: string }> {
    return (await apiClient.post<{ id: string }>(base, body)).data
  },
  async configure(concurrency: number) { await apiClient.put(`${base}/settings`, { concurrency }) },
  async action(batchId: string, action: string, jobId?: string) {
    await apiClient.post(`${base}/${batchId}${jobId ? `/jobs/${jobId}` : ''}/actions`, { action })
  },
  async frame(batchId: string, jobId: string): Promise<{ image: string; live: boolean }> {
    return (await apiClient.get<{ image: string; live: boolean }>(`${base}/${batchId}/jobs/${jobId}/frame`)).data
  },
  async input(batchId: string, jobId: string, input: PlaygroundBrowserInput) {
    await apiClient.post(`${base}/${batchId}/jobs/${jobId}/input`, input)
  },
  async finish(batchId: string, jobId: string) {
    await apiClient.post(`${base}/${batchId}/jobs/${jobId}/finish`, {}, { timeout: 60000 })
  }
}

export interface BatchProxyInput { protocol: string; host: string; port: number; username: string; password: string }

export function parseBatchProxyText(raw: string, protocol = 'http'): BatchProxyInput[] {
  const result: BatchProxyInput[] = []
  for (const [index, row] of raw.split(/\r?\n/).entries()) {
    const line = row.trim()
    if (!line) continue
    let item: BatchProxyInput
    try {
      if (line.includes('://')) {
        const value = new URL(line)
        if (value.search || value.hash || (value.pathname && value.pathname !== '/')) throw new Error()
        item = { protocol: value.protocol.replace(':', ''), host: value.hostname.replace(/^\[|\]$/g, ''), port: Number(value.port || (value.protocol === 'https:' ? 443 : value.protocol === 'http:' ? 80 : 0)), username: decodeURIComponent(value.username), password: decodeURIComponent(value.password) }
      } else {
        const parts = line.split(':')
        if (parts.length !== 2 && parts.length < 4) throw new Error()
        item = { protocol, host: parts[0], port: Number(parts[1]), username: parts[2] || '', password: parts.slice(3).join(':') }
      }
      if (!['http', 'https', 'socks5', 'socks5h'].includes(item.protocol) || !item.host || /\s/.test(item.host) || !Number.isInteger(item.port) || item.port < 1 || item.port > 65535) throw new Error()
    } catch { throw new Error(`第 ${index + 1} 行代理格式不正确`) }
    result.push(item)
    if (result.length > 1000) throw new Error('每次最多导入 1000 条代理')
  }
  if (!result.length) throw new Error('请输入代理')
  return result
}
