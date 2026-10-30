// Package store 是 SQLite 持久层。
//
// 选 SQLite 而非 Postgres：本工具的部署形态是一台小 VPS 上的单进程，
// 数据量是几十个账号加几千条审计记录。引入独立数据库进程只会增加运维负担。
// 所有查询都走本包暴露的方法，便于日后需要时整体替换。
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 实现的 SQLite，免 CGO，可交叉编译

	"ocicore/internal/cryptobox"
)

// ErrNotFound 表示按 ID 查询的记录不存在。
var ErrNotFound = errors.New("store: 记录不存在")

// ErrConflict 表示违反了唯一约束（重复的用户名、租户或账号代号）。
var ErrConflict = errors.New("store: 记录已存在")

// InvalidError 表示调用方提交的输入不合法（PEM 解析不了、指纹对不上、代号格式错……）。
//
// 单独一个类型而不是靠错误文案区分。早先 HTTP 层用「错误串里不含 store: 前缀」
// 来猜这是不是输入问题，结果是任何一处忘了加前缀的内部错误——SQL 报错、磁盘写满、
// 解密失败——都会被当成 400 并原样回显给用户：既泄露内部细节，又让真正的故障
// 看起来像用户填错了。分类必须是显式的，而不是从文案里反推。
type InvalidError struct{ err error }

func (e *InvalidError) Error() string { return e.err.Error() }
func (e *InvalidError) Unwrap() error { return e.err }

// Invalidf 构造一个输入校验错误，保留 %w 的包裹语义。
func Invalidf(format string, args ...any) error {
	return &InvalidError{err: fmt.Errorf(format, args...)}
}

// IsInvalid 报告 err 是否属于输入校验失败。
func IsInvalid(err error) bool {
	var ie *InvalidError
	return errors.As(err, &ie)
}

// Store 持有数据库连接与用于私钥加解密的 Box。
type Store struct {
	db  *sql.DB
	box *cryptobox.Box
}

// Open 打开（必要时创建）数据库并执行建表语句。
func Open(path string, box *cryptobox.Box) (*Store, error) {
	if box == nil {
		return nil, errors.New("store: 缺少 cryptobox，私钥无法加密存储")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("store: 创建数据目录失败: %w", err)
		}
	}

	// WAL 提升并发读性能；busy_timeout 避免瞬时锁冲突直接报错；
	// foreign_keys 让 ON DELETE CASCADE 真正生效（SQLite 默认是关的）；
	// secure_delete 让被删除或覆盖的内容在页内清零，凭据迁移成密文后不在空闲页里留下明文。
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=secure_delete(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: 打开数据库失败: %w", err)
	}

	// SQLite 的写是串行的，放开连接数只会制造锁竞争。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: 连接数据库失败: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: 初始化表结构失败: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}

	s := &Store{db: db, box: box}
	ctx := context.Background()
	// 升级前的 TOTP 密钥与渠道凭据是明文，启动时改写成密文。
	// 失败不阻断启动：未迁移的行仍能按明文读出，下次启动会再试。
	secrets, err := s.migratePlaintextSecrets(ctx)
	if err != nil {
		slog.Warn("加密旧版明文凭据失败，下次启动会重试", "err", err)
	}
	// 数据迁移放在建表与列迁移之后：它要写 proxies 表，也要用到 box 加密。
	// 失败不阻断启动——旧的 proxy_url 字段（已加密）还在，建连时有兜底分支。
	proxies, err := s.migrateLegacyProxies(ctx)
	if err != nil {
		slog.Warn("旧代理配置迁移失败，将继续使用账号上的原字段", "err", err)
	}
	// 收尾放在所有迁移之后：任何一项改过数据都要把 WAL 合回主库。
	if secrets+proxies > 0 {
		s.checkpointAfterMigration(ctx)
	}
	return s, nil
}

// migrations 是对已有库的增量变更。
//
// SQLite 的 ALTER TABLE ADD COLUMN 不支持 IF NOT EXISTS，重复执行会报
// "duplicate column name"。这里把那一种错误当作"已经加过了"放行，
// 其余错误照常抛出——比自己去查 pragma table_info 简单且不易出错。
var migrations = []string{
	`ALTER TABLE accounts ADD COLUMN subscribed_regions TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN home_region TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN email TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN tenancy_name TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN payment_model TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN subscription_state TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN subscription_ends_at INTEGER`,
	`ALTER TABLE instances ADD COLUMN running_since INTEGER`,
	`ALTER TABLE accounts ADD COLUMN subscription_starts_at INTEGER`,
	`ALTER TABLE instances ADD COLUMN note TEXT NOT NULL DEFAULT ''`,
	// hunt_tasks 在这个字段之前就已经发布过了，光靠 CREATE TABLE IF NOT EXISTS
	// 补不上——已有的库里那张表还是旧结构。
	`ALTER TABLE hunt_tasks ADD COLUMN precheck_capacity INTEGER NOT NULL DEFAULT 1`,
	// 代理从"账号的一个字符串字段"升级成独立实体。
	//
	// 旧的 proxy_url 列保留不动：已有部署里可能填过值，启动时会被
	// 迁移进 proxies 表并回填 proxy_id（见 migrateLegacyProxies）。
	// 直接删列会让回滚到旧版本的人丢配置。
	`ALTER TABLE accounts ADD COLUMN proxy_id TEXT`,
	`ALTER TABLE instances ADD COLUMN image_os TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE instances ADD COLUMN baseline_ocpu TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE instances ADD COLUMN secondary_ipv4 TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE instances ADD COLUMN secondary_ipv6 TEXT NOT NULL DEFAULT ''`,
	// 权限自检结果。存 JSON 而不是拆成五列：这五族随时可能增减
	// （P2-4 的计费识别就可能再加一族），拆列意味着每加一族都要迁移一次。
	`ALTER TABLE accounts ADD COLUMN capability_probe TEXT NOT NULL DEFAULT ''`,
	// 换 IP 任务顺带更新 DNS 的结果。
	//
	// 与 result 分成两列而不是拼成一句话：DNS 更新失败不能让一次成功的
	// 换 IP 变成失败——IP 确实换了、机器是通的，只是域名还指着旧地址。
	// 混在一个字段里，用户看到"失败"会以为机器坏了，然后去重启机器。
	`ALTER TABLE ops_tasks ADD COLUMN dns_status TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE ops_tasks ADD COLUMN dns_error TEXT NOT NULL DEFAULT ''`,
}

func migrate(db *sql.DB) error {
	for _, stmt := range migrations {
		if _, err := db.Exec(stmt); err != nil {
			if strings.Contains(err.Error(), "duplicate column name") {
				continue
			}
			return fmt.Errorf("store: 执行迁移失败 (%s): %w", stmt, err)
		}
	}
	return nil
}

// Close 关闭数据库连接。
func (s *Store) Close() error { return s.db.Close() }

// SnapshotTo 用 VACUUM INTO 把当前库写成一份自洽的副本。
//
// 为什么不能直接拷 .db 文件：WAL 模式下已提交的事务可能还留在 -wal 里，
// 只拷主文件会丢掉最近的数据；把 -wal 一起拷过去又可能遇到版本对不上
// （进程还在写）。VACUUM INTO 在一个读事务里产出一份完整的库，
// 不需要停服，也不会带上 WAL。
//
// 目标文件必须不存在——SQLite 会拒绝覆盖已有文件，这是它的保护，
// 我们提前报一个更清楚的错。
func (s *Store) SnapshotTo(ctx context.Context, dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("store: 快照目标已存在: %s", dest)
	}
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", dest); err != nil {
		return fmt.Errorf("store: 生成快照失败: %w", err)
	}
	return nil
}

// DB 暴露底层连接，仅供测试与迁移工具使用。
func (s *Store) DB() *sql.DB { return s.db }

// schema 是完整的建表语句，全部 IF NOT EXISTS，可重复执行。
//
// 时间统一存 INTEGER Unix 秒：SQLite 没有原生时间类型，
// 存文本会引入时区和格式歧义，存整数则完全无歧义。
const schema = `
CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    totp_secret   TEXT NOT NULL DEFAULT '',
    totp_enabled  INTEGER NOT NULL DEFAULT 0,
    -- 记录最近一次成功使用的时间窗，用于拒绝同一验证码的重放
    totp_last_counter INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    -- 存令牌的 SHA-256，库泄露时无法反推出可用的会话令牌
    token_hash    TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- 通过密码但尚未通过 TOTP 的会话为 0，此状态只允许调用 TOTP 校验接口
    totp_verified INTEGER NOT NULL DEFAULT 0,
    ip            TEXT NOT NULL DEFAULT '',
    user_agent    TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    expires_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user    ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS accounts (
    id               TEXT PRIMARY KEY,
    alias            TEXT NOT NULL,
    -- 三字母短代号，与身份色一同构成账号身份。全局唯一
    code             TEXT NOT NULL UNIQUE,
    -- 身份色序号 1..8，对应前端的 acct-1..acct-8
    color_index      INTEGER NOT NULL,
    tenancy_ocid     TEXT NOT NULL UNIQUE,
    user_ocid        TEXT NOT NULL,
    fingerprint      TEXT NOT NULL,
    -- AES-256-GCM 密文与 nonce。明文私钥从不落盘
    key_ciphertext   BLOB NOT NULL,
    key_nonce        BLOB NOT NULL,
    default_region   TEXT NOT NULL,
    compartment_ocid TEXT NOT NULL DEFAULT '',
    proxy_url        TEXT NOT NULL DEFAULT '',
    enabled          INTEGER NOT NULL DEFAULT 1,
    -- unchecked / ok / error
    status           TEXT NOT NULL DEFAULT 'unchecked',
    status_message   TEXT NOT NULL DEFAULT '',
    last_checked_at  INTEGER,
    -- 权限自检结果（JSON）。见 migrations 里的同名列
    capability_probe TEXT NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_logs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    TEXT NOT NULL DEFAULT '',
    action     TEXT NOT NULL,
    account_id TEXT NOT NULL DEFAULT '',
    target     TEXT NOT NULL DEFAULT '',
    detail     TEXT NOT NULL DEFAULT '',
    ip         TEXT NOT NULL DEFAULT '',
    -- ok / fail
    result     TEXT NOT NULL DEFAULT 'ok',
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_logs(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_account ON audit_logs(account_id);

-- 实例缓存。跨账号聚合要对每个（账号 × 区域）发一轮 API 调用，
-- 没有缓存的话用户每次打开列表都得等上好几秒。
CREATE TABLE IF NOT EXISTS instances (
    id                  TEXT PRIMARY KEY,
    account_id          TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    region              TEXT NOT NULL,
    compartment_id      TEXT NOT NULL DEFAULT '',
    display_name        TEXT NOT NULL DEFAULT '',
    availability_domain TEXT NOT NULL DEFAULT '',
    fault_domain        TEXT NOT NULL DEFAULT '',
    shape               TEXT NOT NULL DEFAULT '',
    ocpus               REAL NOT NULL DEFAULT 0,
    memory_gb           REAL NOT NULL DEFAULT 0,
    lifecycle_state     TEXT NOT NULL DEFAULT '',
    image_id            TEXT NOT NULL DEFAULT '',
    public_ip           TEXT NOT NULL DEFAULT '',
    private_ip          TEXT NOT NULL DEFAULT '',
    ipv6                TEXT NOT NULL DEFAULT '',
    vnic_id             TEXT NOT NULL DEFAULT '',
    subnet_id           TEXT NOT NULL DEFAULT '',
    boot_volume_id      TEXT NOT NULL DEFAULT '',
    boot_volume_gb      INTEGER NOT NULL DEFAULT 0,
    boot_volume_vpus    INTEGER NOT NULL DEFAULT 0,
    time_created        INTEGER NOT NULL DEFAULT 0,
    synced_at           INTEGER NOT NULL DEFAULT 0,
    -- 最近一次操作失败的原因。非空时前端在该行浮出错误条，
    -- 对应设计规格里"失败必须可见地回滚"那一条。
    last_error          TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_instances_account ON instances(account_id);
CREATE INDEX IF NOT EXISTS idx_instances_region  ON instances(region);
CREATE INDEX IF NOT EXISTS idx_instances_state   ON instances(lifecycle_state);

-- 全局设置。键值对存储，避免每加一个开关就改一次表结构。
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

-- 通知渠道。config 是各渠道自己的 JSON 配置（token、webhook 地址等），
-- events 是订阅的事件类型 JSON 数组。
CREATE TABLE IF NOT EXISTS notification_channels (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL,
    name       TEXT NOT NULL,
    config     TEXT NOT NULL DEFAULT '{}',
    events     TEXT NOT NULL DEFAULT '[]',
    enabled    INTEGER NOT NULL DEFAULT 1,
    last_error TEXT NOT NULL DEFAULT '',
    last_sent_at INTEGER,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
-- 容量守候（抢机）任务。
--
-- Oracle 的免费 ARM 长期没有容量，LaunchInstance 大概率返回容量不足，
-- 需要在容量释放的那一刻恰好发出请求。任务表让这件事能跨进程重启存活。
CREATE TABLE IF NOT EXISTS hunt_tasks (
    id               TEXT PRIMARY KEY,
    account_id       TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    region           TEXT NOT NULL,
    name             TEXT NOT NULL DEFAULT '',
    -- 创建参数的 JSON 快照。拆成列的话每加一个 OCI 参数都要迁一次表。
    spec             TEXT NOT NULL,
    -- 轮换范围，逗号分隔。空表示该区域全部可用域。
    ads              TEXT NOT NULL DEFAULT '',
    state            TEXT NOT NULL DEFAULT 'running',
    attempts         INTEGER NOT NULL DEFAULT 0,
    interval_seconds INTEGER NOT NULL DEFAULT 60,
    -- 先查容量报告，报告说没货就跳过这一轮，不发创建请求。
    -- 默认开：容量报告是只读的，而 LaunchInstance 才是风控盯的那个。
    precheck_capacity INTEGER NOT NULL DEFAULT 1,
    last_class       TEXT NOT NULL DEFAULT '',
    last_error       TEXT NOT NULL DEFAULT '',
    last_ad          TEXT NOT NULL DEFAULT '',
    last_try_at      INTEGER NOT NULL DEFAULT 0,
    -- 下次可尝试的时刻。调度器只看这一个字段，且它是持久化的——
    -- 只存内存的话每次重启都把退避清零，等于变相提高请求频率。
    next_at          INTEGER NOT NULL DEFAULT 0,
    instance_id      TEXT NOT NULL DEFAULT '',
    max_attempts     INTEGER NOT NULL DEFAULT 0,
    expires_at       INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_hunt_due ON hunt_tasks(state, next_at);
CREATE INDEX IF NOT EXISTS idx_hunt_account ON hunt_tasks(account_id);

-- 容量监控。
--
-- 数据来自 Oracle 官方的容量报告接口（只读，不创建任何资源），不是靠反复调
-- 创建接口试探出来的。每行是「账号 × 区域 × 可用域 × 规格」的一个监控项。
CREATE TABLE IF NOT EXISTS capacity_watches (
    id            TEXT PRIMARY KEY,
    account_id    TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    region        TEXT NOT NULL,
    -- 完整可用域名，不是 AD-1 这种显示用的简写。
    availability_domain TEXT NOT NULL,
    shape         TEXT NOT NULL,
    ocpus         REAL NOT NULL DEFAULT 0,
    memory_gb     REAL NOT NULL DEFAULT 0,
    enabled       INTEGER NOT NULL DEFAULT 1,

    -- AVAILABLE / OUT_OF_HOST_CAPACITY / HARDWARE_NOT_SUPPORTED
    last_status   TEXT NOT NULL DEFAULT '',
    last_count    INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT NOT NULL DEFAULT '',
    -- 最近一次查询的时刻，和最近一次「状态发生变化」的时刻。
    -- 通知只在后者变动时推：天天告诉用户"还是没货"没有意义。
    last_checked_at INTEGER NOT NULL DEFAULT 0,
    last_changed_at INTEGER NOT NULL DEFAULT 0,

    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    UNIQUE(account_id, availability_domain, shape, ocpus, memory_gb)
);
CREATE INDEX IF NOT EXISTS idx_capacity_enabled ON capacity_watches(enabled, last_checked_at);

-- 后台任务（创建实例、换 IP、改配置等长耗时操作）。
--
-- 落库是为了刷新页面、换设备打开时还能看到进度与结果；
-- 进程重启时未结束的行会被标成 interrupted，见 InterruptUnfinishedJobs。
CREATE TABLE IF NOT EXISTS jobs (
    id          TEXT PRIMARY KEY,
    kind        TEXT NOT NULL,
    title       TEXT NOT NULL,
    -- 不加外键：账号删掉之后任务记录仍然有回看价值。
    account_id  TEXT NOT NULL DEFAULT '',
    instance_id TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL,
    progress    INTEGER NOT NULL DEFAULT 0,
    -- 步骤记录的 JSON 数组。
    steps       TEXT NOT NULL DEFAULT '[]',
    summary     TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT '',
    result      TEXT,
    created_at  INTEGER NOT NULL,
    started_at  INTEGER,
    finished_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_jobs_created ON jobs(created_at);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);

-- 代理池。
--
-- 代理是一等实体而不是账号的一个字段：要能独立查看存活状态、独立管理，
-- 而且必须能检测「同一条代理绑给了两个账号」——那种情况下隔离是假的，
-- 反而把两个账号绑在同一个出口 IP 上，凭空制造关联信号。
CREATE TABLE IF NOT EXISTS proxies (
    id            TEXT PRIMARY KEY,
    label         TEXT NOT NULL DEFAULT '',
    -- http / https / socks5
    scheme        TEXT NOT NULL,
    host          TEXT NOT NULL,
    port          INTEGER NOT NULL,
    username      TEXT NOT NULL DEFAULT '',
    -- 密码与 OCI 私钥同等待遇：AES-256-GCM 加密，AAD 绑定本行 id，
    -- 明文从不落盘。无密码时两列为 NULL。
    pass_ciphertext BLOB,
    pass_nonce      BLOB,
    enabled       INTEGER NOT NULL DEFAULT 1,

    -- ok / fail / unknown
    last_status     TEXT NOT NULL DEFAULT 'unknown',
    last_latency_ms INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT NOT NULL DEFAULT '',
    last_region     TEXT NOT NULL DEFAULT '',
    last_checked_at INTEGER NOT NULL DEFAULT 0,
    last_ok_at      INTEGER NOT NULL DEFAULT 0,

    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    -- 同一个出口不该重复录入。scheme 也算进来：同一台机器的 http 与
    -- socks5 端口是两条独立的路。
    UNIQUE(scheme, host, port, username)
);

-- 调用预算：每个账号在每个小时窗口里对 OCI 发起了多少次调用。
--
-- 落库而不是只放内存，是因为重启恰好发生在任务出问题、调用量最大的时候：
-- 只存内存的话，重启一次预算就清零，降频立刻消失，于是「重启一下好像就好了」
-- 变成一个自我实现的错误结论。
--
-- 不设外键：计数是旁路统计，账号刚被删掉时的一次写入失败不该让用户的
-- 正常操作跟着失败。旧行由 PurgeRiskBudget 定期清理。
CREATE TABLE IF NOT EXISTS risk_budget (
    -- 账号的本地 ID（不是租户 OCID）
    account_id   TEXT NOT NULL,
    -- 小时对齐的 Unix 秒，与 riskguard.WindowStart 的算法必须一致
    window_start INTEGER NOT NULL,
    calls        INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (account_id, window_start)
);
CREATE INDEX IF NOT EXISTS idx_risk_budget_window ON risk_budget(window_start);

-- ── 通用运维任务 ──────────────────────────────────────────────
-- 与 hunt_tasks 并列而不是合并（方案 ADR-4 原本要求合并，实施时改了，见计划 §7.1）：
-- 抢机的字段（可用域轮换、规格快照、IPv6 预处理）与运维任务的字段（阶段、
-- 进度）几乎没有交集，合表会变成两张表的并集加一堆半空列，而每个查询都要
-- 多带一串 kind = ? 分支。共享的是调度器（internal/tasksched），不是存储。
CREATE TABLE IF NOT EXISTS ops_tasks (
    id               TEXT PRIMARY KEY,
    -- hunt / change_ip_cidr / open_ports / enable_500m / rescue / bulk
    kind             TEXT NOT NULL,
    account_id       TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    instance_id      TEXT NOT NULL DEFAULT '',
    -- 给人看的标签。方案里的建表语句没有这一列，但列表与审计都需要一个
    -- 可读的标识，靠 kind + id 拼出来的东西没人认得。
    name             TEXT NOT NULL DEFAULT '',
    spec             TEXT NOT NULL DEFAULT '{}',
    state            TEXT NOT NULL DEFAULT 'pending',
    -- 分阶段任务（救援）当前停在哪一步
    stage            TEXT NOT NULL DEFAULT '',
    progress         TEXT NOT NULL DEFAULT '{}',
    attempts         INTEGER NOT NULL DEFAULT 0,
    max_attempts     INTEGER NOT NULL DEFAULT 30,
    interval_seconds INTEGER NOT NULL DEFAULT 20,
    last_error       TEXT NOT NULL DEFAULT '',
    result           TEXT NOT NULL DEFAULT '',
    expires_at       INTEGER NOT NULL DEFAULT 0,
    next_at          INTEGER NOT NULL DEFAULT 0,
    started_at       INTEGER NOT NULL DEFAULT 0,
    finished_at      INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ops_due     ON ops_tasks(state, next_at);
CREATE INDEX IF NOT EXISTS idx_ops_account ON ops_tasks(account_id, state);

-- 账号标签。一个账号可以有多个，标签是自由文本。
--
-- 单独建表而不是在 accounts 上加一列逗号分隔的字符串：筛选要按标签反查
-- 账号（WHERE tag = ?），逗号串只能全表扫再在内存里切；而标签的增删改
-- 在串上都是"读-改-写"，两个并发请求会互相覆盖。
CREATE TABLE IF NOT EXISTS account_tags (
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    tag        TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (account_id, tag)
);
-- 反查方向：从标签找账号。列表页的筛选器走这条路。
CREATE INDEX IF NOT EXISTS idx_account_tags_tag ON account_tags(tag);

-- 账号的附加信息。与 accounts 分表是因为这些字段是可选的、可空的，
-- 且大多数账号根本不会填——塞进主表会让 accounts 的列继续膨胀，
-- 而每次列表查询都要为几个通常为空的列付代价。
CREATE TABLE IF NOT EXISTS account_meta (
    account_id  TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    -- 分组名。空串表示"未分组"，前端把这类账号归到最后一组。
    group_name  TEXT NOT NULL DEFAULT '',
    note        TEXT NOT NULL DEFAULT '',
    -- 账号到期时间。试用号到期后实例会被回收，列表页要提前提醒。
    expires_at  INTEGER,
    -- 手动排序权重，大的在前。默认 0 表示不干预，退回到创建时间顺序。
    sort_weight INTEGER NOT NULL DEFAULT 0,
    -- 预留给抢机页的「已知悉」标记。当前只做存取，没有任何流程依赖它。
    hunt_ack_at INTEGER,
    updated_at  INTEGER NOT NULL
);

-- 账号健康度。单独建表而不是往 accounts 上加列，理由与 account_meta 相同：
-- 这是"最近一次校验的结论"，每次校验都会整体改写，与账号配置的读写频率
-- 完全不同；混在一起会让 accounts 的每次 UPDATE 都要顺带重写这几列。
CREATE TABLE IF NOT EXISTS account_health (
    account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    -- 最近一次校验成功的时刻。NULL 表示从来没成功过。
    last_ok_at INTEGER,
    -- 最近一次失败的分类，取 ociclient.Class 的名字（AuthFailed / Throttled …）。
    -- 成功时清空，所以"有 last_error_at 但没有 class"只可能是刚恢复。
    last_error_class TEXT NOT NULL DEFAULT '',
    last_error_at INTEGER,
    last_error_message TEXT NOT NULL DEFAULT '',
    -- 连续凭据类失败次数。非凭据类结果（成功、限流、配额、网络抖动）会把它归零，
    -- 因为它对"这份密钥还能不能用"没有信息量。见 accountsvc.Next。
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    -- 是否已经确认凭据不可用。连续失败达到阈值才置 1，单次抖动不置。
    fatal INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
);

-- 顶部红区块要按 fatal 筛，列表页要按 level 排，两者都会扫这张表。
CREATE INDEX IF NOT EXISTS idx_account_health_fatal ON account_health(fatal);

-- Cloudflare 区（zone）。token 与账号私钥、代理密码同一套信封加密，
-- 因为它等价于「这个域名的全部 DNS 写权限」。
--
-- account_id 可以为空：一个 CF 账号下的域名可能同时指向多个 OCI 账号里的
-- 机器，硬绑到某个 OCI 账号上会逼用户为同一个 zone 录两遍 token。
CREATE TABLE IF NOT EXISTS cf_zones (
    id               TEXT PRIMARY KEY,
    account_id       TEXT NOT NULL DEFAULT '',
    zone_id          TEXT NOT NULL,
    zone_name        TEXT NOT NULL DEFAULT '',
    token_ciphertext BLOB,
    token_nonce      BLOB,
    enabled          INTEGER NOT NULL DEFAULT 1,
    last_sync_at     INTEGER NOT NULL DEFAULT 0,
    last_error       TEXT NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);

-- 同一个 zone 只能录一次。重复录入会让同一个域名有两条 token，
-- 之后改 A 记录时用哪一条完全看遍历顺序。
CREATE UNIQUE INDEX IF NOT EXISTS idx_cf_zones_zone ON cf_zones(zone_id);

-- 一条绑定的含义是「这条 A 记录跟随这台机器」。换 IP 之后同步的就是它。
--
-- 刻意不用外键约束到 instances：实例被终止后，用户往往还想保留这条绑定
-- （下次同名机器起来时手动同步一次），级联删除会把配置一起带走。
CREATE TABLE IF NOT EXISTS cf_records (
    id           TEXT PRIMARY KEY,
    zone_pk      TEXT NOT NULL,
    record_id    TEXT NOT NULL,
    name         TEXT NOT NULL,
    type         TEXT NOT NULL DEFAULT 'A',
    proxied      INTEGER NOT NULL DEFAULT 0,
    ttl          INTEGER NOT NULL DEFAULT 1,
    account_id   TEXT NOT NULL DEFAULT '',
    instance_id  TEXT NOT NULL DEFAULT '',
    last_ip      TEXT NOT NULL DEFAULT '',
    last_sync_at INTEGER NOT NULL DEFAULT 0,
    last_error   TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_cf_records_rid ON cf_records(zone_pk, record_id);
CREATE INDEX IF NOT EXISTS idx_cf_records_inst ON cf_records(instance_id);

-- IP 情报缓存。离线库（GeoLite2）查一次是内存操作，本不需要缓存；
-- 这张表存在的理由是「查不到时也要留痕」：哪个 IP 查过、用的哪个库版本、
-- 结果是空的——排障时要能区分「这个 IP 确实没有归属地」和「库没加载」。
CREATE TABLE IF NOT EXISTS ip_cache (
    ip           TEXT PRIMARY KEY,
    country      TEXT NOT NULL DEFAULT '',
    country_code TEXT NOT NULL DEFAULT '',
    region       TEXT NOT NULL DEFAULT '',
    city         TEXT NOT NULL DEFAULT '',
    latitude     REAL NOT NULL DEFAULT 0,
    longitude    REAL NOT NULL DEFAULT 0,
    asn          TEXT NOT NULL DEFAULT '',
    org          TEXT NOT NULL DEFAULT '',
    -- source 记录这条是谁填的：mmdb（离线库）或 manual（用户手填）。
    -- 空字符串表示查过但没查到。
    source       TEXT NOT NULL DEFAULT '',
    updated_at   INTEGER NOT NULL DEFAULT 0
);
`

// newID 生成 16 位十六进制的主键。对本工具的规模而言，128 位熵远超需求。
func newID() (string, error) {
	buf := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("store: 生成 ID 失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func nowUnix() int64 { return time.Now().Unix() }

// unixToTime 把数据库里的 Unix 秒转成 time.Time。
//
// 0 一律还原成零值 Time，而不是 1970-01-01。这是与 timeToUnix 的对称要求：
// 零值 Time 存进去是 0，读回来必须还是零值，否则 IsZero() 永远为假。
//
// 这个不对称曾经真实咬过人：hunt_tasks.expires_at 存 0 表示"不设到期"，
// 读回来变成 1970 之后 shouldStop 判定"任务已到期"，于是不设到期时间的守候
// 任务一上线就被判死，而且错误文案看起来完全合理，没人会怀疑是序列化问题。
func unixToTime(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0)
}

// nullUnixToTime 处理可空的时间列。
func nullUnixToTime(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0)
	return &t
}

// isUniqueViolation 判断错误是否来自唯一约束冲突。
// modernc 驱动不暴露结构化错误码，只能匹配消息文本。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE")
}

// PurgeExpiredSessions 删除过期会话。调用方应定期执行。
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, nowUnix())
	if err != nil {
		return 0, fmt.Errorf("store: 清理过期会话失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
