import { apiClient } from '../client'

export interface PlaygroundWarmupConfig {
  enabled: boolean
  target: number
  max_cached: number
  idle_minutes: number
}
export interface PlaygroundWarmupAccount {
  account_id: number
  state: string
  request_preparation_ms?: number
  last_request_warm?: boolean
  duration_ms: number
  prepared_at?: string
  retry_at?: string
  error?: string
}
export interface PlaygroundWarmupState {
  config: PlaygroundWarmupConfig
  eligible: number
  cached: number
  light_cached: number
  ready: number
  active_requests: number
  warming_account_id?: number
  memory_headroom_mb: number
  paused_reason?: string
  accounts: PlaygroundWarmupAccount[]
}
const base = '/admin/accounts/playground/warmup'
export const playgroundWarmupAPI = {
  async get(signal?: AbortSignal): Promise<PlaygroundWarmupState> {
    return (await apiClient.get<PlaygroundWarmupState>(base, { signal })).data
  },
  async configure(config: PlaygroundWarmupConfig): Promise<PlaygroundWarmupState> {
    return (await apiClient.put<PlaygroundWarmupState>(base, config)).data
  }
}
