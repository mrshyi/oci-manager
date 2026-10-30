package logstream

import (
	"context"
	"log/slog"
	"strings"
)

// prefixedAttr 记住一个属性是在哪一层分组里被加进来的。
//
// 只存 slog.Attr 是不够的：`WithAttrs` 之后再 `WithGroup`，先加的那批
// 属于外层。摊平成「键 → 字符串」时要还原这个层次，否则
// `slog.With("a",1).WithGroup("db").With("b",2)` 会被显示成 `db.a`。
type prefixedAttr struct {
	prefix string
	attr   slog.Attr
}

// Handler 包装一个 slog.Handler：先脱敏，再分别写往原输出与环形缓冲。
//
// 顺序不能反。先写缓冲再脱敏，明文就进过内存了；而脱敏必须在**进入任何
// 存储**之前完成——包括这块只活几秒的内存。
type Handler struct {
	inner  slog.Handler
	ring   *Ring
	attrs  []prefixedAttr // 已脱敏，来自 WithAttrs
	groups []string
}

// NewHandler 把 inner 包一层。ring 为 nil 时退化为纯脱敏包装。
func NewHandler(inner slog.Handler, ring *Ring) *Handler {
	return &Handler{inner: inner, ring: ring}
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	msg := Redact(r.Message)

	attrs := make(map[string]string, r.NumAttrs()+len(h.attrs))
	for _, pa := range h.attrs {
		collectAttr(attrs, pa.prefix, pa.attr)
	}
	// rec 只放本次记录自带的属性。WithAttrs 传下来的那批已经在
	// h.inner（= inner.WithAttrs(red)）里了，再塞一遍会在输出里重复。
	rec := slog.NewRecord(r.Time, r.Level, msg, r.PC)
	prefix := strings.Join(h.groups, ".")
	r.Attrs(func(a slog.Attr) bool {
		red := redactAttr(a)
		collectAttr(attrs, prefix, red)
		rec.AddAttrs(red)
		return true
	})

	h.ring.Push(Entry{
		Time:  r.Time,
		Level: r.Level.String(),
		Msg:   msg,
		Attrs: attrs,
	})
	return h.inner.Handle(ctx, rec)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	prefix := strings.Join(h.groups, ".")
	red := redactAll(attrs)
	next := &Handler{
		inner:  h.inner.WithAttrs(red),
		ring:   h.ring,
		groups: h.groups,
	}
	// 复制而不是共享底层数组：slog 会复用 WithAttrs 传入的切片，
	// 直接 append 可能把调用方的数组写坏。
	next.attrs = append([]prefixedAttr{}, h.attrs...)
	for _, a := range red {
		next.attrs = append(next.attrs, prefixedAttr{prefix: prefix, attr: a})
	}
	return next
}

func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &Handler{
		inner:  h.inner.WithGroup(name),
		ring:   h.ring,
		attrs:  h.attrs,
		groups: append(append([]string{}, h.groups...), name),
	}
}

func redactAll(attrs []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, redactAttr(a))
	}
	return out
}

// redactAttr 递归脱敏一个属性。
//
// 只看字符串值：数字、布尔、时间里没有凭据。但 Group 要钻进去——
// `slog.Group("creds", "password", x)` 是很自然的写法，跳过它等于漏掉。
func redactAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, RedactValue(a.Key, v.String()))
	case slog.KindGroup:
		return slog.Group(a.Key, attrsToAny(redactAll(v.Group()))...)
	default:
		return slog.Attr{Key: a.Key, Value: v}
	}
}

func attrsToAny(attrs []slog.Attr) []any {
	out := make([]any, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, a)
	}
	return out
}

// collectAttr 把属性摊平成「键 → 字符串」，供 SSE 输出。
//
// 分组用点号连接：`db.addr` 比嵌套 JSON 更省，而且前端只要原样显示。
func collectAttr(dst map[string]string, prefix string, a slog.Attr) {
	key := a.Key
	if prefix != "" {
		key = prefix + "." + key
	}
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		for _, m := range v.Group() {
			collectAttr(dst, key, m)
		}
		return
	}
	// 空键是 slog 用来表达「这一项没有名字」的，丢掉而不是存一个空键。
	if key == "" {
		return
	}
	dst[key] = v.String()
}
