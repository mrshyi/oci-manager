/**
 * 进程日志的 SSE 客户端。
 *
 * 与事件流（events.ts）分开：那条流推的是"实例状态变了"这类业务事件，
 * 这条推的是服务自己的日志。两者的重连策略、消费方式、生命周期都不同
 * ——日志只在打开日志页时才需要连，事件流是全局常连的。
 */

export interface LogEntry {
  seq: number
  time: string
  level: string
  msg: string
  attrs?: Record<string, string>
}

export interface LogStreamHandlers {
  /** 一条日志（含连接建立时补发的历史） */
  onEntry?: (entry: LogEntry) => void
  /** 连接建立。参数为是否重连 */
  onOpen?: (reconnected: boolean) => void
  onError?: () => void
}

/** 建立日志流连接，返回断开函数。 */
export function connectLogs(handlers: LogStreamHandlers): () => void {
  let source: EventSource | null = null
  let opened = false
  let closed = false

  const open = () => {
    if (closed) return
    source = new EventSource('/api/logs/stream', { withCredentials: true })

    source.onopen = () => {
      const reconnected = opened
      opened = true
      handlers.onOpen?.(reconnected)
    }

    source.onerror = () => {
      // EventSource 自带重连。这里只通知 UI 显示"已断开"，
      // 不主动 close——主动关掉就再也连不回来了。
      handlers.onError?.()
    }

    source.addEventListener('log', (raw) => {
      try {
        handlers.onEntry?.(JSON.parse((raw as MessageEvent).data) as LogEntry)
      } catch {
        // 单条解析失败没有补救办法，丢掉即可：下一条还会来。
      }
    })
  }

  open()

  return () => {
    closed = true
    source?.close()
    source = null
  }
}
