import { GetBootstrap } from '../generated/wailsjs/go/main/DesktopApp'

export type CapabilityDTO = { id: string; label: string; available: boolean }

export type DataStatusDTO = {
  ready: boolean
  schemaVersion: number
  mode: string
  loggingReady: boolean
}

export type BootstrapDTO = {
  apiVersion: string
  name: string
  version: string
  state: 'CREATED' | 'RUNNING' | 'STOPPED'
  data: DataStatusDTO
  capabilities: CapabilityDTO[]
}

export async function loadBootstrap(): Promise<BootstrapDTO> {
  const value = await GetBootstrap()
  if (!value || value.apiVersion !== 'v1' || !value.data || !Array.isArray(value.capabilities)) {
    throw new Error('UI_CONTRACT_INVALID: bootstrap payload')
  }
  return value as BootstrapDTO
}
