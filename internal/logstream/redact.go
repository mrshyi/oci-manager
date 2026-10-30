// Package logstream 把进程日志同时写往标准输出与一份内存环形缓冲，
// 供面板的「运行日志」页实时查看。
//
// 三条设计约束，都写在这里因为它们贯穿整个包：
//
//  1. **脱敏在写入端**。展示端脱敏等于明文已经在内存里等着被 dump——
//     环形缓冲、panic 的栈、core 文件都能拿到它。所以 Redact 在
//     Handler.Handle 里、在进环之前执行，而不是在 SSE 输出时。
//
//  2. **缓冲有界**。5000 条固定容量，写满覆盖最旧的。日志是排障工具，
//     不是审计账本；要留存请用 docker logs。无界缓冲只会在跑几天后
//     把内存吃干净，而且是在最不该出问题的时候。
//
//  3. **慢消费者不阻塞写日志**。每个订阅者一个带缓冲的 channel，满了就丢。
//     让一条卡住的 SSE 连接把整个进程的日志堵死，是最糟的失败方式。
package logstream

import (
	"regexp"
	"strings"
)

// mask 是替换后的占位符。
//
// 不用空串：`password=` 后面什么都没有，和「这个字段本来就不存在」
// 看起来一样；排障时需要知道"这里有过东西，只是被我们藏了"。
const mask = "***"

// Redact 把字符串里疑似凭据的部分替换成 ***。
//
// 覆盖 8 类：
//  1. PEM 私钥块（-----BEGIN ... PRIVATE KEY-----）
//  2. privateKey / privateKeyPem
//  3. password / passwd / pwd
//  4. token
//  5. secret
//  6. apiKey / api_key
//  7. Authorization 头与 Bearer / Basic 凭据
//  8. 非 OCID 的长十六进制串（32 位以上）
//
// 另外顺手处理 URL 里的 user:pass@——代理地址会带密码，而它出现在
// 错误信息里的频率比想象中高。
func Redact(s string) string {
	if s == "" {
		return s
	}
	s = pemRe.ReplaceAllString(s, "-----BEGIN PRIVATE KEY-----"+mask+"-----END PRIVATE KEY-----")
	s = urlCredRe.ReplaceAllString(s, "://"+mask+"@")
	s = bearerRe.ReplaceAllString(s, "$1 "+mask)
	s = kvRe.ReplaceAllString(s, "${1}${2}"+mask+"${4}")
	return redactLongHex(s)
}

// RedactValue 按「键 + 值」脱敏，用于结构化日志的属性。
//
// 为什么需要单独一个函数：属性里的键和值是分开的，
// `slog.String("password", "hunter2")` 交给 Redact 时只有 "hunter2"——
// 没有 `password=` 这个上下文，任何正则都认不出它是密码。
// 所以键名本身必须参与判定。
func RedactValue(key, val string) string {
	if IsSensitiveKey(key) {
		return mask
	}
	return Redact(val)
}

// sensitiveKeyParts 是键名里出现即视为凭据的片段。
//
// 用「包含」而不是「等于」：真实的键名是 clientSecret、csrfToken、
// dbPassword、privateKeyPem 这类组合词，逐个枚举永远会漏。
// 代价是 tokenCount 这种无害字段也会被抹掉——对一个持有全部云账号
// 控制权的面板来说，这个方向的误差是可以接受的。
var sensitiveKeyParts = []string{
	"privatekey", "password", "passwd", "pwd", "secret",
	"token", "apikey", "authorization", "credential",
}

// IsSensitiveKey 判断一个键名是否指向凭据。
//
// 先归一化：忽略大小写与 _ - . 空格，让 private_key / privateKey /
// PRIVATE-KEY 走同一条判定。
func IsSensitiveKey(key string) bool {
	k := strings.ToLower(key)
	k = keyNormalizer.Replace(k)
	for _, part := range sensitiveKeyParts {
		if strings.Contains(k, part) {
			return true
		}
	}
	return false
}

var keyNormalizer = strings.NewReplacer("_", "", "-", "", ".", "", " ", "")

// redactLongHex 处理长十六进制串，但放过 OCID。
//
// 为什么不能简单地正则替换：OCID 的尾段就是长串小写字母，常常整段都是
// 十六进制字符（`ocid1.tenancy.oc1..aaaaaaaabbbb...`）。无差别替换会把
// 每一条带 OCID 的日志都糊成 `ocid1.tenancy.oc1..***`，而 OCID 恰恰是
// 排障时最需要看到的东西——它不是凭据，是标识符。
//
// 所以先把 OCID 的位置标出来，再逐段判断。相邻字符是字母数字的也跳过：
// 那说明它是一个更长 token 的一部分，替换掉会留下半截更难读的东西。
func redactLongHex(s string) string {
	locs := hexRe.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return s
	}
	protected := ocidRe.FindAllStringIndex(s, -1)

	var b strings.Builder
	b.Grow(len(s))
	prev := 0
	for _, loc := range locs {
		start, end := loc[0], loc[1]
		if overlapsAny(start, end, protected) || adjacentAlnum(s, start, end) {
			continue
		}
		b.WriteString(s[prev:start])
		b.WriteString(mask)
		prev = end
	}
	if prev == 0 {
		return s
	}
	b.WriteString(s[prev:])
	return b.String()
}

func overlapsAny(start, end int, spans [][]int) bool {
	for _, sp := range spans {
		if start < sp[1] && sp[0] < end {
			return true
		}
	}
	return false
}

func adjacentAlnum(s string, start, end int) bool {
	if start > 0 && isAlnum(s[start-1]) {
		return true
	}
	if end < len(s) && isAlnum(s[end]) {
		return true
	}
	return false
}

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

var (
	// pemRe 匹配 PEM 私钥块。中间那段 base64 可以跨行。
	pemRe = regexp.MustCompile(`-----BEGIN[A-Z ]*PRIVATE KEY-----[\s\S]*?-----END[A-Z ]*PRIVATE KEY-----`)

	// urlCredRe 匹配 URL 里的 user:pass@。代理地址是主要来源。
	urlCredRe = regexp.MustCompile(`://[^/\s:@]+:[^/\s@]+@`)

	// bearerRe 匹配 Bearer / Basic 后面的凭据本体。
	//
	// 要求 6 位以上：`Bearer` 后面跟个短词多半是在描述而不是真凭据，
	// 过度匹配会让日志变得读不懂，而读不懂的日志等于没有日志。
	bearerRe = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{6,}`)

	// kvRe 匹配 key: value 与 key=value 两种写法，键名大小写不敏感。
	//
	// 值分两种：被引号包住的（JSON、Go 的 %q）与裸的。引号要保留在输出里，
	// 否则 JSON 日志被脱敏后就不是合法 JSON 了——而它本来是要被解析的。
	kvRe = regexp.MustCompile(`(?i)((?:privatekey(?:pem)?|private_key(?:_pem)?|password|passwd|pwd|token|secret|api_?key|authorization|auth_token)\s*"?\s*[:=]\s*)(")?([^"\s,;&}]+)(")?`)

	// hexRe 匹配长十六进制串。32 位是下限：AES-256 的密钥正好 64 个字符，
	// 而 MD5 之类的摘要 32 个字符起——两者都不该出现在日志里。
	hexRe = regexp.MustCompile(`[0-9a-fA-F]{32,}`)

	// ocidRe 匹配 OCID，用来把它们从长十六进制串里摘出来。
	ocidRe = regexp.MustCompile(`ocid1\.[A-Za-z0-9._-]+`)
)
