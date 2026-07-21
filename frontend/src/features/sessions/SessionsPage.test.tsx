import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import * as api from './api'
import { SessionsPage } from './SessionsPage'

vi.mock('./api')
vi.mock('../../lib/desktop', () => ({ userFacingError: () => '操作失败' }))

const sessionId = '019aa000-0000-7000-8000-000000000001'
const roomId = '019aa000-0000-7000-8000-000000000002'
const segmentId = '019aa000-0000-7000-8000-000000000003'
const artifactId = '019aa000-0000-7000-8000-000000000004'
const eventId = '019aa000-0000-7000-8000-000000000005'

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })
  return render(<QueryClientProvider client={client}><SessionsPage /></QueryClientProvider>)
}

describe('SessionsPage', () => {
  beforeEach(() => {
    vi.mocked(api.listPlaybackSessions).mockResolvedValue({ version: 1, items: [{
      id: sessionId, roomConfigId: roomId, roomAlias: '测试直播间', title: '夏季新品场',
      status: 'completed', recordingStatus: 'completed', startedAt: 1_700_000_000_000,
      endedAt: 1_700_000_060_000, mediaEpochAt: 1_700_000_000_000,
      captureOffsetMs: 0, clockSource: 'media', integrityScore: 0.98, sessionMediaState: 'completed',
    }] })
    vi.mocked(api.getPlaybackSession).mockResolvedValue({ version: 1, session: {
      id: sessionId, roomConfigId: roomId, roomAlias: '测试直播间', title: '夏季新品场',
      status: 'completed', recordingStatus: 'completed', startedAt: 1_700_000_000_000,
      endedAt: 1_700_000_060_000, mediaEpochAt: 1_700_000_000_000,
      captureOffsetMs: 0, clockSource: 'media', integrityScore: 0.98, sessionMediaState: 'completed',
    } })
    vi.mocked(api.listPlaybackEvents).mockResolvedValue({ version: 1, items: [{
      id: eventId, ingestSequence: 1, role: 'source', kind: 'chat', receivedAt: 1_700_000_005_000,
      sessionOffsetMs: 5_000, clockConfidence: 1, displayName: '观众', content: '讲解一下尺码', parseStatus: 'parsed',
    }] })
    vi.mocked(api.listPlaybackGaps).mockResolvedValue({ version: 1, items: [] })
    vi.mocked(api.listPlaybackMedia).mockResolvedValue({ version: 1, items: [{
      id: segmentId, sequence: 1, container: 'mkv', videoCodec: 'h264', audioCodec: 'aac',
      startedAt: 1_700_000_000_000, endedAt: 1_700_000_060_000,
      durationMs: 60_000, sizeBytes: 1024, status: 'complete', timelineStartMs: 0, timelineEndMs: 60_000,
      artifacts: [{
        id: artifactId, mediaSegmentId: segmentId, kind: 'playback_mp4', container: 'mp4', codec: 'h264',
        durationMs: 60_000, sizeBytes: 1000, sampleRate: 0, channels: 0, status: 'complete', directPlayback: true,
      }], playbackArtifactId: artifactId,
    }] })
    vi.mocked(api.locatePlaybackMedia).mockResolvedValue({
      version: 1, sessionId, requestedOffsetMs: 5_000, adjustedOffsetMs: 5_000,
      state: 'playback_mp4', segmentPlaybackMs: 5_000, playbackArtifactId: artifactId,
      segment: {
        id: segmentId, sequence: 1, container: 'mkv', videoCodec: 'h264', audioCodec: 'aac',
        startedAt: 1_700_000_000_000, endedAt: 1_700_000_060_000,
        durationMs: 60_000, sizeBytes: 1024, status: 'complete', timelineStartMs: 0, timelineEndMs: 60_000,
        artifacts: [], playbackArtifactId: artifactId,
      },
    })
    vi.mocked(api.playbackMediaURL).mockImplementation((id) => `/playback/media/${id}`)
  })

  it('opens an analysis candidate at its requested timeline offset', async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })
    render(<QueryClientProvider client={client}><SessionsPage initialSessionId={sessionId} initialOffsetMs={12_000} /></QueryClientProvider>)
    await screen.findByRole('heading', { name: '夏季新品场' })
    await waitFor(() => expect(api.locatePlaybackMedia).toHaveBeenCalledWith(sessionId, 12_000))
  })
  it('renders historical detail and seeks an event into a verified media URL', async () => {
    const user = userEvent.setup()
    renderPage()
    expect(await screen.findByRole('heading', { name: '夏季新品场' })).toBeInTheDocument()
    expect(screen.getByText('完整性')).toBeInTheDocument()
    expect(screen.getByLabelText('统一时间轴')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /观众/ }))
    expect(api.locatePlaybackMedia).toHaveBeenCalledWith(sessionId, 5_000)
    const player = await screen.findByLabelText('场次视频')
    expect(player).toHaveAttribute('src', `/playback/media/${artifactId}`)
  })
})
