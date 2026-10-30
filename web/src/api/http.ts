/**
 * HTTP 客户端。所有后端调用都走这里。
 *
 * 三件事集中在这一层处理，业务代码不必重复：
 *  1. 写操作自动带 CSRF 头（后端要求，见 docs/API.md）
 *  2. 错误统一成 ApiError，保留原始 OCI 错误码与处理建议
 *  3. 401 广播一次全局事件，由路由层决定跳登录还是弹两步验证
 *  4. 危险操作要求复核时（403 danger_totp_*）向用户要验证码，带上后重发
 */

/** 后端约定的 CSRF 请求头。浏览器不允许跨源请求携带自定义头。 */
const CSRF_HEADER = 'X-OCI-Tools'
const CSRF_VALUE = '1'

/** 401 时广播的事件名。 */
export const AUTH_REQUIRED_EVENT = 'oci:auth-required'

/** 危险操作附带验证码的请求头，与后端 dangerCodeHeader 一致。 */
const DANGER_CODE_HEADER = 'X-OCI-TOTP-Code'

/** 后端要求（或拒绝了）危险操作验证码时的错误码。 */
const DANGER_CODES = new Set(['danger_totp_required', 'danger_totp_invalid', 'danger_totp_reused'])

/**
 * 向用户索取验证码。返回 null 表示用户取消。
 *
 * 由 store 注册：HTTP 层不该知道弹窗长什么样。没注册时直接把错误抛出去。
 */
type DangerCodeProvider = (message: string) => Promise<string | null>
let dangerCodeProvider: DangerCodeProvider | null = null

export function setDangerCodeProvider(fn: DangerCodeProvider): void {
  dangerCodeProvider = fn
}

export interface ApiErrorBody {
  code?: string
  message?: string
  ociCode?: string
  advice?: string
}

/**
 * ApiError 保留后端给的全部错误信息。
 *
 * ociCode 是 Oracle 的原始错误码，UI 上必须原样展示且可复制——
 * 用户排障时要拿它去搜索。advice 是可直接显示的中文建议。
 */
export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly ociCode?: string
  readonly advice?: string

  constructor(status: number, body: ApiErrorBody) {
    super(body.message || `请求失败（HTTP ${status}）`)
    this.name = 'ApiError'
    this.status = status
    this.code = body.code || 'unknown'
    this.ociCode = body.ociCode
    this.advice = body.advice
  }

  /** 供 Toast 直接显示的一行说明。 */
  get display(): string {
    return this.ociCode ? `${this.ociCode} · ${this.message}` : this.message
  }

  /** 需要用户完成两步验证。 */
  get needsTotp(): boolean {
    return this.code === 'totp_required'
  }

  /** 该操作缺少确认参数，前端应弹确认框后重试。 */
  get needsConfirm(): boolean {
    return this.code === 'confirm_required'
  }
}

type Method = 'GET' | 'POST' | 'PATCH' | 'PUT' | 'DELETE'

interface RequestOptions {
  /** 查询参数。undefined / null / '' 的项会被丢弃。 */
  query?: Record<string, string | number | boolean | undefined | null>
  body?: unknown
  signal?: AbortSignal
}

function buildUrl(path: string, query?: RequestOptions['query']): string {
  if (!query) return path
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(query)) {
    if (value === undefined || value === null || value === '') continue
    params.set(key, String(value))
  }
  const qs = params.toString()
  return qs ? `${path}?${qs}` : path
}

/**
 * 危险操作被要求复核时，在这里补上验证码重发——业务代码不必知道这件事。
 *
 * 验证码错误或已被使用时后端会再次拒绝，带着原因重新问一次，直到成功或用户取消。
 */
async function request<T>(method: Method, path: string, opts: RequestOptions = {}): Promise<T> {
  return retryWithDangerCode(code => send<T>(method, path, opts, code))
}

/**
 * 危险操作的验证码重试循环。抽出来是因为三种请求形态（JSON、二进制、
 * 表单）都要用它——复制三遍迟早会有一处忘了补。
 */
async function retryWithDangerCode<T>(attempt: (code?: string) => Promise<T>): Promise<T> {
  let code: string | undefined
  for (;;) {
    try {
      return await attempt(code)
    } catch (err) {
      if (!(err instanceof ApiError) || !DANGER_CODES.has(err.code) || !dangerCodeProvider) throw err
      const next = await dangerCodeProvider(err.message)
      if (next === null) throw err
      code = next
    }
  }
}

/** 统一的请求头构造。三种请求形态共用。 */
function buildHeaders(method: Method, opts: RequestOptions, dangerCode?: string): Record<string, string> {
  const headers: Record<string, string> = { Accept: 'application/json' }
  if (dangerCode) headers[DANGER_CODE_HEADER] = dangerCode
  if (method !== 'GET') {
    headers[CSRF_HEADER] = CSRF_VALUE
    // 表单的 Content-Type 由浏览器带 boundary 生成，这里不能覆盖。
    if (opts.body !== undefined && !(opts.body instanceof FormData)) {
      headers['Content-Type'] = 'application/json'
    }
  }
  return headers
}

/** 把非 2xx 的响应转成 ApiError。401 顺带广播一次。 */
async function toApiError(response: Response): Promise<ApiError> {
  const text = await response.text()
  let payload: unknown
  try {
    payload = text ? JSON.parse(text) : undefined
  } catch {
    payload = undefined
  }
  const body = (payload ?? {}) as ApiErrorBody
  if (response.status === 401) {
    window.dispatchEvent(new CustomEvent(AUTH_REQUIRED_EVENT, {
      detail: { code: body.code ?? 'unauthenticated' }
    }))
  }
  return new ApiError(response.status, body)
}

/** 发一次请求，返回原始 Response。网络层失败转成 ApiError(0)。 */
async function fire(method: Method, path: string, opts: RequestOptions,
                    dangerCode: string | undefined, body: BodyInit | undefined): Promise<Response> {
  try {
    return await fetch(buildUrl(path, opts.query), {
      method,
      headers: buildHeaders(method, opts, dangerCode),
      credentials: 'same-origin',
      body,
      signal: opts.signal
    })
  } catch (err) {
    // 网络层失败没有状态码。用 0 表示，让上层能与业务错误区分开。
    if ((err as Error)?.name === 'AbortError') throw err
    throw new ApiError(0, { code: 'network', message: '无法连接到服务，请检查网络或后端是否在运行' })
  }
}

function jsonBody(opts: RequestOptions): BodyInit | undefined {
  if (opts.body === undefined) return undefined
  if (opts.body instanceof FormData) return opts.body
  return JSON.stringify(opts.body)
}

async function send<T>(method: Method, path: string, opts: RequestOptions, dangerCode?: string): Promise<T> {
  // 演示站（GitHub Pages）没有后端，所有请求交给浏览器里的模拟实现。
  // VITE_DEMO 在构建时就被替换成常量，正常构建里这个分支连同 demo 模块一起被摇掉。
  if (import.meta.env.VITE_DEMO === 'true') {
    const { demoSend } = await import('@/demo/server')
    return demoSend<T>(method, path, opts.query, opts.body)
  }

  const response = await fire(method, path, opts, dangerCode, jsonBody(opts))
  if (response.status === 204) return undefined as T
  if (!response.ok) throw await toApiError(response)

  const text = await response.text()
  let payload: unknown = undefined
  if (text) {
    try {
      payload = JSON.parse(text)
    } catch {
      payload = undefined
    }
  }
  return payload as T
}

/**
 * 二进制响应（备份导出、审计 CSV）。
 *
 * 不能走 send：它会把响应体当 JSON 解析，而备份是密文、审计是 CSV，
 * JSON.parse 失败后返回 undefined——用户会拿到一个「成功但没有内容」
 * 的结果，然后以为备份是空的。
 */
async function sendBlob(method: Method, path: string, opts: RequestOptions,
                        dangerCode?: string): Promise<Blob> {
  // 演示站（GitHub Pages）没有后端，导出交给浏览器里的模拟实现。
  // 与 send 同一个判断：VITE_DEMO 在构建时被替换成常量，正常构建整段被摇掉。
  if (import.meta.env.VITE_DEMO === 'true') {
    const { demoSend } = await import('@/demo/server')
    return demoSend<Blob>(method, path, opts.query, opts.body)
  }

  const response = await fire(method, path, opts, dangerCode, jsonBody(opts))
  if (!response.ok) throw await toApiError(response)
  return response.blob()
}

export const http = {
  get: <T>(path: string, opts?: RequestOptions) => request<T>('GET', path, opts),
  post: <T>(path: string, body?: unknown, opts?: RequestOptions) =>
    request<T>('POST', path, { ...opts, body }),
  patch: <T>(path: string, body?: unknown, opts?: RequestOptions) =>
    request<T>('PATCH', path, { ...opts, body }),
  put: <T>(path: string, body?: unknown, opts?: RequestOptions) =>
    request<T>('PUT', path, { ...opts, body }),
  del: <T>(path: string, opts?: RequestOptions) => request<T>('DELETE', path, opts),

  /** 下载二进制。备份导出用 */
  postBlob: (path: string, body?: unknown, opts?: RequestOptions) =>
    retryWithDangerCode(code => sendBlob('POST', path, { ...opts, body }, code)),

  /**
   * 下载二进制（GET）。审计导出用。
   *
   * 不用复核验证码：导出是只读操作，后端不会走 danger 那一套。
   */
  getBlob: (path: string, opts?: RequestOptions) => sendBlob('GET', path, opts ?? {}),

  /** 上传表单。恢复备份用 */
  postForm: <T>(path: string, form: FormData, opts?: RequestOptions) =>
    retryWithDangerCode(code => send<T>('POST', path, { ...opts, body: form }, code))
}

/** 把任意异常转成可显示的一行文本。 */
export function errorText(err: unknown): string {
  if (err instanceof ApiError) return err.display
  if (err instanceof Error) return err.message
  return String(err)
}

/** 取出错误里的处理建议，没有则返回空串。 */
export function errorAdvice(err: unknown): string {
  return err instanceof ApiError && err.advice ? err.advice : ''
}
