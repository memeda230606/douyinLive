import { GetBootstrap } from '../generated/wailsjs/go/main/DesktopApp'
import { bootstrapSchema, contractError } from '../lib/contracts'

export type { BootstrapDTO, DataStatusDTO } from '../lib/contracts'

export async function loadBootstrap() {
  const value = await GetBootstrap()
  const parsed = bootstrapSchema.safeParse(value)
  if (!parsed.success) throw contractError('bootstrap', value)
  return parsed.data
}
