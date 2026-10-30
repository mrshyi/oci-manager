// Package config 集中处理运行期配置。全部通过环境变量注入，不引入配置文件格式。
package config

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config 是服务的完整配置。
type Config struct {
	// Addr 是监听地址。默认只听回环——这个面板持有所有租户的控制权，
	// 直接暴露公网风险过高。需要外部访问时应由用户显式配置，并自行套 TLS。
	Addr string

	// StaticDir 指向一个前端产物目录。非空时优先于嵌入的资源，
	// 便于前端开发期用真实后端调试而不必每次重新编译 Go。
	StaticDir string

	// DataDir 存放 SQLite 数据库与主密钥文件。
	DataDir string

	// MasterKeyHex 从环境变量直接注入主密钥（十六进制）。
	// 留空则退回到 DataDir 下的密钥文件，文件不存在时自动生成。
	MasterKeyHex string

	// AllowNewMasterKey 允许在「库中已有加密数据」时仍然新建主密钥。
	//
	// 默认关闭。正常路径下，主密钥文件缺失而库里还有密文，说明密钥丢了或者
	// 只恢复了库——此时新建一把密钥会让那些数据永久解不开，所以启动会被拒绝。
	// 只有用户确认库里没有要保留的东西时才该打开它。
	AllowNewMasterKey bool

	// SessionTTL 是会话有效期，每次请求滑动续期。
	SessionTTL time.Duration

	// TrustedProxies 是允许提供 X-Forwarded-For / X-Forwarded-Proto 的代理网段。
	//
	// 空表示不信任任何代理，此时一律用 TCP 连接的来源地址。非空时只采信
	// 「来源落在这些网段内」的请求所带的头——早先这里是个 bool，打开就等于
	// 相信所有人，而任何人都能伪造该头来绕过登录失败限流。
	TrustedProxies []netip.Prefix

	// RiskBudgetEnabled 控制是否统计每账号的调用量并按超预算降频。
	//
	// 默认开启。关掉它的用途只有一个：统计逻辑本身出问题时，一句话退回
	// 「不降频」的行为，而不用回滚整个二进制。
	RiskBudgetEnabled bool

	// GeoIPDir 是离线 IP 库（MaxMind GeoLite2）所在目录。
	//
	// 留空表示「不启用 IP 情报」。这个库需要用户自己去 MaxMind 注册并下载，
	// 不能随程序分发（许可不允许），所以缺失是常态而不是异常——把它做成
	// 启动前置条件会让绝大多数人第一次就跑不起来。
	GeoIPDir string

	// NotifyAllowedNets 是通知 webhook 额外允许访问的网段。
	// 默认禁止通知请求访问回环、内网与链路本地地址，确需推到内网服务时在这里显式放行。
	NotifyAllowedNets []netip.Prefix
}

// DBPath 返回数据库文件路径。
//
// 文件名保留改名前的 oci-tools.db：里面存着所有账号的加密私钥，
// 主密钥在同一个目录下。改文件名的收益只是好看，代价是任何一次
// 迁移出错都可能让人打不开自己的账号。
func (c Config) DBPath() string { return filepath.Join(c.DataDir, "oci-tools.db") }

// MasterKeyPath 返回主密钥文件路径。
func (c Config) MasterKeyPath() string { return filepath.Join(c.DataDir, "master.key") }

// legacyPrefix 是最初那版（oci-tools）的环境变量前缀。
//
// 中间还叫过一阵 OCIPerch，但那个名字只活了一个提交、没有任何部署，
// 所以不为它保留兼容——多留一个前缀就多一份要一直维护的历史包袱。
//
// 继续识别它，是因为已经部署的实例大多把 OCI_TOOLS_MASTER_KEY 写进了
// systemd unit 或 docker-compose。改名不该让别人的服务在下次重启时
// 突然读不到主密钥——那等于所有已保存的 OCI 私钥都解不开了。
const legacyPrefix = "OCI_TOOLS_"

// lookupEnv 依次尝试新前缀与旧前缀，返回第一个非空值。
func lookupEnv(name string) string {
	if v := strings.TrimSpace(os.Getenv(envPrefix + name)); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv(legacyPrefix + name))
}

// envPrefix 是当前的环境变量前缀。
const envPrefix = "OCICORE_"

// Load 从环境变量读取配置并填充默认值。
func Load() (Config, error) {
	cfg := Config{
		// 端口与前端开发服务器的代理目标保持一致（见 web/vite.config.ts）。
		Addr:         orDefault(lookupEnv("ADDR"), "127.0.0.1:8080"),
		DataDir:      orDefault(lookupEnv("DATA_DIR"), "./data"),
		MasterKeyHex: lookupEnv("MASTER_KEY"),
		StaticDir:    lookupEnv("STATIC_DIR"),
		SessionTTL:   12 * time.Hour,
		// 默认关：只有在明确写 true 时才允许覆盖已有密文的主密钥。
		AllowNewMasterKey: parseBool(lookupEnv("ALLOW_NEW_MASTER_KEY")),
		// 默认开：显式配成 false 才关。
		RiskBudgetEnabled: !isFalse(lookupEnv("RISK_BUDGET_ENABLED")),
		GeoIPDir:          lookupEnv("GEOIP_DIR"),
	}

	// 可信代理：优先读显式网段；没有则回落到旧的 TRUST_PROXY 布尔开关。
	if v := lookupEnv("TRUSTED_PROXIES"); v != "" {
		nets, err := parseAllowedNets(v)
		if err != nil {
			return cfg, fmt.Errorf("config: %sTRUSTED_PROXIES 无效: %w", envPrefix, err)
		}
		if err := rejectUnboundedPrefix(nets); err != nil {
			return cfg, fmt.Errorf("config: %sTRUSTED_PROXIES %w", envPrefix, err)
		}
		cfg.TrustedProxies = nets
	} else if parseBool(lookupEnv("TRUST_PROXY")) {
		cfg.TrustedProxies = legacyTrustedProxies()
	}

	if v := lookupEnv("SESSION_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("config: %sSESSION_TTL 格式无效 (%q): %w", envPrefix, v, err)
		}
		if d < time.Minute {
			return cfg, fmt.Errorf("config: %sSESSION_TTL 不能短于 1 分钟", envPrefix)
		}
		cfg.SessionTTL = d
	}

	if v := lookupEnv("NOTIFY_ALLOWED_NETS"); v != "" {
		nets, err := parseAllowedNets(v)
		if err != nil {
			return cfg, fmt.Errorf("config: %sNOTIFY_ALLOWED_NETS 无效: %w", envPrefix, err)
		}
		cfg.NotifyAllowedNets = nets
	}

	abs, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return cfg, fmt.Errorf("config: 解析数据目录失败: %w", err)
	}
	cfg.DataDir = abs

	// IP 库默认放在数据目录下的 geoip/。显式给了 GEOIP_DIR 就用它，
	// 但也转成绝对路径：相对路径在 systemd 与 docker 下的含义完全不同。
	if cfg.GeoIPDir == "" {
		cfg.GeoIPDir = filepath.Join(abs, "geoip")
	} else if g, err := filepath.Abs(cfg.GeoIPDir); err == nil {
		cfg.GeoIPDir = g
	}

	return cfg, nil
}

// ListensPublicly 报告监听地址是否会接受来自外部网络的连接，
// 用于在启动日志里给出安全提示。
func (c Config) ListensPublicly() bool {
	host := c.Addr
	if idx := strings.LastIndex(host, ":"); idx >= 0 {
		host = host[:idx]
	}
	host = strings.Trim(host, "[]")
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return false
	default:
		return true
	}
}

// isFalse 只在显式写成假值时返回 true。
//
// 与 parseBool 的区别是默认值方向：这里"没配"要当成真，所以不能用
// parseBool（它把空串当假）。风控相关的开关默认值必须偏保守。
//
// 顺带接受 no / off：strconv.ParseBool 只认 true/false/1/0 那几种写法，
// 而人在写部署脚本时更习惯 no / off。写成无法解析的值一律按"没关"处理——
// 配置写错不该顺手把保护关掉。
func isFalse(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "false", "0", "no", "off":
		return true
	}
	return false
}

func parseBool(v string) bool {
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	return err == nil && b
}

func orDefault(v, fallback string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return fallback
}

// parseAllowedNets 解析逗号分隔的网段或单个 IP，例如 "10.0.0.0/8,192.168.1.5"。
func parseAllowedNets(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "/") {
			p, err := netip.ParsePrefix(part)
			if err != nil {
				return nil, fmt.Errorf("网段 %q 无效: %w", part, err)
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(part)
		if err != nil {
			return nil, fmt.Errorf("地址 %q 无效: %w", part, err)
		}
		out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
	}
	return out, nil
}

// rejectUnboundedPrefix 拒绝 0.0.0.0/0 与 ::/0。
//
// 写这两个网段的人几乎一定是想表达「我信任我的代理」，但它的实际含义是
// 「我信任全世界」——配上 X-Forwarded-For 就等于把限流桶的钥匙交出去。
// 这种配置错误不会报错、不会有症状，只会在被爆破之后才从审计日志里
// 看到一串编造的 IP，所以必须在启动时就拦住。
func rejectUnboundedPrefix(nets []netip.Prefix) error {
	for _, p := range nets {
		if p.Bits() == 0 {
			return fmt.Errorf("不接受 %s：它等于信任所有来源，"+
				"X-Forwarded-For 将可被任意伪造。请填写代理的实际网段。", p)
		}
	}
	return nil
}

// legacyTrustedProxies 是旧 TRUST_PROXY=true 对应的网段。
//
// 旧语义是「信任一切」，直接沿用等于保留了漏洞，直接删掉又会让已经部署的
// 实例在升级后突然开始记录代理 IP、限流按代理 IP 计数（等于全站共用一个桶）。
// 折中成「回环 + RFC1918」：反向代理绝大多数就跑在本机或同一私有网络里，
// 这样行为基本不变，而来自公网的请求无论如何都无法伪造来源。
//
// 私有网段内的主机仍可伪造，所以这只是过渡措施——启动日志里会提示改用
// TRUSTED_PROXIES 显式声明。
func legacyTrustedProxies() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"127.0.0.0/8", "::1/128",
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		"fc00::/7",
	} {
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p)
		}
	}
	return out
}
