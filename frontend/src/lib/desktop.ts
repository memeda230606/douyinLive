import {
  CancelUpdateDownload,
  CheckForUpdate,
  ClearRoomCookie,
  CreateRoom,
  DeleteRoom,
  GetRoomStatus,
  GetSettings,
  GetUpdateStatus,
  InstallPreparedUpdate,
  ListRooms,
  PrepareUpdate,
  SetRoomCookie,
  SelectRecordingDirectory,
  StartMonitoring,
  StopMonitoring,
  UpdateRoom,
  UpdateSettings,
} from '../generated/wailsjs/go/main/DesktopApp'
import { room as roomModels } from '../generated/wailsjs/go/models'
import {
  contractError,
  roomSchema,
  roomsSchema,
  roomStatusSchema,
  settingsSchema,
  updateStatusSchema,
  type RoomConfig,
  type RoomFormValues,
  type RoomRuntimeStatus,
  type SettingsFormValues,
} from './contracts'

function parse<T>(schema: { safeParse: (value: unknown) => { success: boolean; data?: T } }, contract: string, value: unknown): T {
  const parsed = schema.safeParse(value)
  if (!parsed.success) throw contractError(contract, value)
  return parsed.data as T
}

function roomInput(values: RoomFormValues) {
  return {
    liveId: values.liveId,
    alias: values.alias,
    monitorEnabled: values.monitorEnabled,
    recordEnabled: values.recordEnabled,
    recordingProfile: { quality: values.quality, segmentMinutes: values.segmentMinutes },
  }
}

export async function listRooms(): Promise<RoomConfig[]> {
  return parse(roomsSchema, 'rooms', await ListRooms())
}

export async function getRoomStatus(id: string): Promise<RoomRuntimeStatus> {
  return parse(roomStatusSchema, 'room status', await GetRoomStatus(id))
}

export async function saveRoom(id: string | undefined, values: RoomFormValues): Promise<RoomConfig> {
  const input = roomInput(values)
  const raw = id ? await UpdateRoom(id, new roomModels.UpdateRoomInput(input))
    : await CreateRoom(new roomModels.CreateRoomInput(input))
  const saved = parse(roomSchema, 'room', raw)
  if (values.cookie) await SetRoomCookie({ roomId: saved.id, cookie: values.cookie })
  return saved
}

export async function removeRoom(id: string, deleteData = false): Promise<void> {
  await DeleteRoom(id, deleteData)
}

export async function clearRoomCookie(id: string): Promise<void> {
  await ClearRoomCookie(id)
}

export async function startMonitoring(id: string): Promise<void> {
  await StartMonitoring(id)
}

export async function stopMonitoring(id: string): Promise<void> {
  await StopMonitoring(id)
}

export async function getSettings() {
  return parse(settingsSchema, 'settings', await GetSettings())
}

export async function selectRecordingDirectory(current: string): Promise<string> {
  return SelectRecordingDirectory(current)
}

export async function saveSettings(values: SettingsFormValues) {
  return parse(settingsSchema, 'settings', await UpdateSettings(values))
}

export async function getUpdateStatus() {
  return parse(updateStatusSchema, 'update status', await GetUpdateStatus())
}

export async function checkForUpdate() {
  return parse(updateStatusSchema, 'update status', await CheckForUpdate())
}

export async function prepareUpdate() {
  return parse(updateStatusSchema, 'update status', await PrepareUpdate())
}

export async function cancelUpdateDownload() {
  return parse(updateStatusSchema, 'update status', await CancelUpdateDownload())
}

export async function installPreparedUpdate() {
  return parse(updateStatusSchema, 'update status', await InstallPreparedUpdate())
}

export function userFacingError(error: unknown): string {
  const message = error instanceof Error ? error.message : String(error)
  if (message.includes('ROOM_ALREADY_EXISTS')) return '该直播间已经存在。'
  if (message.includes('ROOM_INPUT_INVALID')) return '房间信息格式无效，请检查表单。'
  if (message.includes('STORAGE_NOT_WRITABLE')) return '录制目录不可写，请更换目录。'
  if (message.includes('STORAGE_DIRECTORY_PICKER_FAILED')) return '无法打开系统目录选择器，请手动输入绝对路径。'
  if (message.includes('SETTINGS_INVALID')) return '设置值无效，请检查后重试。'
  if (message.includes('UI_CONTRACT_INVALID')) return '桌面服务返回了无法识别的数据，请重启应用。'
  if (message.includes('CAPTURE_FINALIZING')) return '直播场次正在收尾，请稍候再试。'
  if (message.includes('UPDATE_BUSY')) return '直播、录制、重连或收尾期间不能下载或安装更新。'
  if (message.includes('UPDATE_SIGNATURE')) return '更新签名无效，已拒绝该更新。'
  if (message.includes('UPDATE_HASH_MISMATCH')) return '更新安装包校验失败，临时文件已删除。'
  if (message.includes('UPDATE_DISK_FULL')) return '磁盘空间不足，无法安全安装更新。'
  if (message.includes('UPDATE_CHECK_FAILED')) return '暂时无法连接更新服务，请稍后重试。'
  if (message.includes('UPDATE_DOWNLOAD_CANCELLED')) return '更新下载已取消。'
  if (message.includes('UPDATE_')) return '自动更新操作失败，请稍后重试或查看诊断信息。'
  return '操作失败，请稍后重试。'
}
