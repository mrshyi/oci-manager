package httpapi

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"ocicore/internal/logstream"
)

// logReplay 是建立连接时先补发的条数。
//
// 200 条：够看清「刚才发生了什么」，又不至于让首次渲染卡一下。
// 补发是必须的——SSE 只能推「之后」的日志，而用户打开这一页
// 往往正是因为刚刚出了事。
const logReplay = 200

// handleLogStream 把进程日志以 SSE 推给前端。
//
// 数据来自 logstream.Ring（5000 条环形缓冲），脱敏已经在写入端做完，
// 这里不再做任何处理——展示端脱敏等于明文已经在内存里等着被 dump。
func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	if s.logs == nil {
		writeError(w, http.StatusServiceUnavailable, "logs_unavailable",
			"日志缓冲未启用（服务以未接线的模式启动）")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "no_streaming", "当前服务器不支持流式响应")
		return
	}

	// 解除写超时。理由见 handleEvents：WriteTimeout 是绝对截止时间，
	// 不是空闲计时器，SSE 这种永不结束的响应会被服务端自己掐断。
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		slog.Debug("日志流无法解除写超时", "err", err)
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()

	// 先订阅再快照。
	//
	// 反过来会丢掉这两步之间的日志——而那正好是用户刚点开页面、
	// 最想看到的那几十毫秒。代价是可能有重复，靠 Seq 去重。
	events, unsubscribe := s.logs.Subscribe()
	defer unsubscribe()

	var lastSeq uint64
	for _, e := range s.logs.Snapshot(logReplay) {
		if !writeLogEvent(w, e) {
			return
		}
		lastSeq = e.Seq
	}
	flusher.Flush()

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case e, ok := <-events:
			if !ok {
				return
			}
			// 补发与订阅的重叠部分：Seq 已经推过的直接跳过。
			if e.Seq <= lastSeq {
				continue
			}
			if !writeLogEvent(w, e) {
				return
			}
			flusher.Flush()

		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// writeLogEvent 写一条日志事件，返回是否还需要继续。
func writeLogEvent(w http.ResponseWriter, e logstream.Entry) bool {
	data, err := json.Marshal(e)
	if err != nil {
		// 单条序列化失败不该断掉整条流——跳过它继续推后面的。
		return true
	}
	_, err = fmt.Fprintf(w, "event: log\ndata: %s\n\n", data)
	return err == nil
}
