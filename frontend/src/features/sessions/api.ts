import {
  GetPlaybackSession,
  ListPlaybackEvents,
  ListPlaybackGaps,
  ListPlaybackMedia,
  ListPlaybackSessions,
  LocatePlaybackMedia,
} from '../../generated/wailsjs/go/main/DesktopApp'
import { playback } from '../../generated/wailsjs/go/models'
import { contractError } from '../../lib/contracts'
import {
  eventPageSchema,
  gapPageSchema,
  mediaLocationSchema,
  mediaPageSchema,
  sessionPageSchema,
  sessionResultSchema,
} from './contracts'

function parse<T>(schema: { safeParse: (value: unknown) => { success: boolean; data?: T } }, name: string, value: unknown): T {
  const result = schema.safeParse(value)
  if (!result.success) throw contractError(name, value)
  return result.data as T
}

export async function listPlaybackSessions(statuses: string[], cursor = '') {
  const value = await ListPlaybackSessions(
    new playback.SessionFilter({ Statuses: statuses }),
    new playback.PageRequest({ Limit: 50, Cursor: cursor }),
  )
  return parse(sessionPageSchema, 'playback sessions', value)
}

export async function getPlaybackSession(sessionId: string) {
  return parse(sessionResultSchema, 'playback session', await GetPlaybackSession(sessionId))
}

export async function listPlaybackEvents(sessionId: string, cursor = '') {
  const value = await ListPlaybackEvents(
    new playback.EventFilter({ SessionID: sessionId, Roles: ['source'] }),
    new playback.PageRequest({ Limit: 100, Cursor: cursor }),
  )
  return parse(eventPageSchema, 'playback events', value)
}

export async function listPlaybackGaps(sessionId: string, cursor = '') {
  const value = await ListPlaybackGaps(
    new playback.GapFilter({ SessionID: sessionId }),
    new playback.PageRequest({ Limit: 100, Cursor: cursor }),
  )
  return parse(gapPageSchema, 'playback gaps', value)
}

export async function listPlaybackMedia(sessionId: string, cursor = '') {
  const value = await ListPlaybackMedia(
    new playback.MediaFilter({ SessionID: sessionId }),
    new playback.PageRequest({ Limit: 100, Cursor: cursor }),
  )
  return parse(mediaPageSchema, 'playback media', value)
}

export async function locatePlaybackMedia(sessionId: string, offsetMs: number) {
  const value = await LocatePlaybackMedia(new playback.MediaLocationRequest({
    SessionID: sessionId,
    SessionOffsetMS: Math.max(0, Math.trunc(offsetMs)),
  }))
  return parse(mediaLocationSchema, 'playback media location', value)
}

export function playbackMediaURL(artifactId: string): string {
  return `/playback/media/${encodeURIComponent(artifactId)}`
}
