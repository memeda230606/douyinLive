import { create } from 'zustand'

import type { UpdateStatus } from '../lib/contracts'

type UpdateState = {
  status?: UpdateStatus
  setStatus: (status: UpdateStatus) => void
  reset: () => void
}

export const useUpdateStore = create<UpdateState>((set) => ({
  status: undefined,
  setStatus: (status) => set({ status }),
  reset: () => set({ status: undefined }),
}))
