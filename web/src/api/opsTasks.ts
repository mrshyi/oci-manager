/**
 * 运维任务（`internal/opssvc`）的接口。
 *
 * 单独一个文件而不是塞进 `api/index.ts`：`api/index.ts` 是上游文件，每次
 * 合并都会改它，把本地的东西都堆进去会让冲突越滚越大。
 *
 * 这里与 `api/index.ts` 里的 `jobs` 是两套任务系统，别混用：
 *   - `jobs` 是上游 jobsvc，一次性长操作，进度 0–100，取消即止
 *   - `opsTasks` 是本地的 opssvc，可重试、可暂停恢复、可能跑几十轮
 */
import { http } from './http'
import type {
  CreateChangeIPTaskInput,
  OpsTaskAcceptedDTO,
  OpsTaskDTO,
  OpsTaskListDTO
} from './types'

export type OpsTaskAction = 'pause' | 'resume' | 'cancel' | 'retry'

export interface OpsTaskFilter {
  accountId?: string
  kind?: string
  state?: string
}

function query(f?: OpsTaskFilter): string {
  if (!f) return ''
  const p = new URLSearchParams()
  if (f.accountId) p.set('accountId', f.accountId)
  if (f.kind) p.set('kind', f.kind)
  if (f.state) p.set('state', f.state)
  const s = p.toString()
  return s ? `?${s}` : ''
}

export const opsTasks = {
  list: (f?: OpsTaskFilter) => http.get<OpsTaskListDTO>(`/api/tasks${query(f)}`),

  get: (id: string) => http.get<{ task: OpsTaskDTO }>(`/api/tasks/${encodeURIComponent(id)}`),

  /**
   * pause / resume / cancel / retry。状态不允许时后端回 409，前端提示刷新。
   *
   * notice 是后端配给每个动作的一句说明，直接展示比前端自己编一句准——
   * 尤其 cancel：用户要知道的是「已发出去的云端操作不会回滚」。
   */
  action: (id: string, action: OpsTaskAction) =>
    http.post<{ task: OpsTaskDTO; notice: string }>(
      `/api/tasks/${encodeURIComponent(id)}/${action}`,
      {}
    ),

  /** 删除记录。走 danger 复核，HTTP 层会自动要验证码后重发 */
  remove: (id: string) =>
    http.del<{ ok: boolean; notice: string }>(`/api/tasks/${encodeURIComponent(id)}`),

  /**
   * 创建「按网段反复换 IP」的任务。
   *
   * 与 `instances.changeIp`（上游，换一次就结束）不同：这个会一直换到命中
   * 目标网段、用尽次数或超时为止。走 danger 复核，HTTP 层会自动向用户要
   * 验证码再重发。
   */
  createChangeIP: (input: CreateChangeIPTaskInput) =>
    http.post<OpsTaskAcceptedDTO>('/api/ops/change-ip', input)
}
