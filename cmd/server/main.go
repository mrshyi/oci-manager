// Command server 是 OCI Core 的单二进制服务：HTTP 接口 + 嵌入式前端。
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"ocicore/internal/backup"
	"ocicore/internal/config"
	"ocicore/internal/cryptobox"
	"ocicore/internal/geoip"
	"ocicore/internal/httpapi"
	"ocicore/internal/instancesvc"
	"ocicore/internal/logstream"
	"ocicore/internal/ociconn"
	"ocicore/internal/riskguard"
	"ocicore/internal/store"
	"ocicore/internal/web"
)

// version 由构建时通过 -ldflags "-X main.version=..." 注入。
var version = "dev"

// 编译期确认 store 的计数器满足 riskguard 的接口。
//
// 断言放在 main 而不是 store 里：接线是在这里完成的，store 不需要知道
// riskguard 的存在。放在 store 里会让「持久层依赖业务包」这个方向反过来。
var _ riskguard.Counter = (*store.RiskBudgetCounter)(nil)

func main() {
	// 日志同时写往 stderr 与一份内存环形缓冲（面板的「运行日志」页读它）。
	//
	// 脱敏在 logstream.Handler 里、在进环之前完成：展示端脱敏等于明文
	// 已经在内存里等着被 dump。
	//
	// 输出目的地保持 stderr 不变——改成 stdout 会改变 docker logs 的
	// 行为（虽然两者都被采集），而加一个缓冲不该顺带改动别的。
	logs := logstream.NewRing(logstream.DefaultCapacity)
	slog.SetDefault(slog.New(logstream.NewHandler(
		slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}),
		logs,
	)))

	if err := run(logs); err != nil {
		slog.Error("启动失败", "err", err)
		os.Exit(1)
	}
}

func run(logs *logstream.Ring) error {
	httpapi.Version = version

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// 暂存的备份恢复要在这里生效：必须在读主密钥与打开数据库**之前**。
	//
	// 放在后面就晚了——主密钥已经从磁盘读进内存，而恢复可能刚把它换掉；
	// 数据库更是被打开着，此时替换文件会把库写坏。
	if applied, err := backup.ApplyPendingRestore(cfg.DBPath(), cfg.MasterKeyPath()); err != nil {
		return err
	} else if applied {
		slog.Warn("已应用暂存的备份恢复。替换前的库已另存为 *.before-restore-*")
		if cfg.MasterKeyHex != "" {
			// 环境变量优先于文件，此时恢复包里那份主密钥不会生效。
			slog.Warn("主密钥来自环境变量，恢复包里的 master.key 已被忽略")
		}
	}

	// 主密钥优先取环境变量，便于容器化部署时由外部密钥管理注入；
	// 未提供则回落到数据目录下的密钥文件，首次运行时自动生成。
	var masterKey []byte
	if cfg.MasterKeyHex != "" {
		if masterKey, err = cryptobox.DecodeKey(cfg.MasterKeyHex); err != nil {
			return err
		}
		slog.Info("主密钥来自环境变量", "var", "OCICORE_MASTER_KEY（或兼容的 OCI_TOOLS_MASTER_KEY）")
	} else {
		// 生成新密钥之前先确认「这台机器上本来就没有需要旧密钥的数据」。
		if err := ensureMasterKeyMayBeCreated(cfg); err != nil {
			return err
		}
		if masterKey, err = cryptobox.LoadOrCreateMasterKey(cfg.MasterKeyPath()); err != nil {
			return err
		}
		slog.Info("主密钥文件就绪", "path", cfg.MasterKeyPath())
	}

	box, err := cryptobox.New(masterKey)
	if err != nil {
		return err
	}

	st, err := store.Open(cfg.DBPath(), box)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := warnOnFirstRun(st, cfg); err != nil {
		return err
	}

	conns := ociconn.New(st)

	// 调用预算：统计每账号每小时对 OCI 的调用次数，供任务层降频。
	//
	// 计数落库（risk_budget 表）而不是只放内存：重启恰好发生在任务出问题、
	// 调用量最大的时候，只存内存的话重启一次预算就清零，降频立刻消失，
	// 「重启一下好像就好了」于是变成一个自我实现的错误结论。
	//
	// OCICORE_RISK_BUDGET_ENABLED=false 可整体关掉，退回「不降频」的行为。
	var budget *riskguard.Guard
	if cfg.RiskBudgetEnabled {
		budget = riskguard.New(store.NewRiskBudgetCounter(st))
		conns.SetRiskGuard(budget)
	} else {
		slog.Warn("调用预算已关闭，抢机等循环任务不会因调用量过大而降频")
	}

	instances := instancesvc.New(st, conns, instancesvc.NewBus())

	// 离线 IP 库。没装库是常态而不是异常——GeoLite2 要用户自己去 MaxMind
	// 注册下载，不能随程序分发。所以这里只降级，不中断启动。
	//
	// 但「库文件在、却读不出来」必须说出来：那种情况的表现是「所有 IP 都
	// 查不到归属地」，看起来像本工具的 bug，实际是文件下坏了。
	geoDB, err := geoip.Open(cfg.GeoIPDir)
	if err != nil {
		slog.Warn("离线 IP 库加载失败，IP 归属地将退回按区域估算", "dir", cfg.GeoIPDir, "err", err)
	}
	if geoDB != nil {
		defer func() { _ = geoDB.Close() }()
		slog.Info("已加载离线 IP 库",
			"dir", cfg.GeoIPDir, "builtAt", geoDB.Status().CityBuiltAt.Format("2006-01-02"))
	} else {
		slog.Info("未配置离线 IP 库，分布图将按 OCI 区域估算位置", "dir", cfg.GeoIPDir)
	}

	api := httpapi.New(httpapi.Deps{
		Store:     st,
		Config:    cfg,
		Conns:     conns,
		Instances: instances,
		Budget:    budget,
		Logs:      logs,
		GeoIP:     geoDB,
	})
	handler, err := buildHandler(api, cfg)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 重启恢复：把上次进程退出时停在 running 的运维任务重置为 pending。
	//
	// 进程被杀时正在执行的任务，数据库里停在 running，而那个 goroutine 已经
	// 不存在了。不重置的话这些任务永远不会再被捞到——界面显示「运行中」，
	// 实际上一动不动，而且没有任何错误可看。
	//
	// 放在启动调度器之前，否则第一轮 tick 会先看到一批永远不该是 running 的行。
	if n, err := st.ResetRunningOpsTasks(ctx); err != nil {
		// 失败不阻断启动：任务跑不起来是局部问题，服务起不来是全局问题。
		slog.Warn("重置运行中的运维任务失败", "err", err)
	} else if n > 0 {
		slog.Info("已将上次中断的运维任务重新排队", "count", n)
	}

	go purgeSessionsPeriodically(ctx, st)

	// 定期复查凭据。间隔从设置里读，改动一分钟内生效，不需要重启。
	go api.RunAccountChecker(ctx)
	go api.RunAuditPruner(ctx)
	go api.RunRiskBudgetPruner(ctx)
	// 容量守候。调度器只看持久化的 next_at，重启后退避进度原样恢复——
	// 只存内存的话每次重启都把退避清零，等于变相提高了对 Oracle 的请求频率。
	go api.RunHunter(ctx)
	// 运维任务（放行端口、换 IP、救援……）的调度循环。与抢机共用调度器，
	// 但按 kind 分派到各自的处理器，互不干扰。
	go api.RunOps(ctx)
	go api.RunCapacityMonitor(ctx)

	// 进程重启会丢掉内存里的轮询协程。不恢复的话，那些停在"关机中"的行
	// 要等到下一轮全量同步才会更新，用户会以为界面卡死了。
	instances.ResumeWatches(ctx)

	settings, err := st.Settings(ctx)
	if err != nil {
		return err
	}
	instances.StartBackgroundSync(ctx, time.Duration(settings.SyncIntervalMinutes)*time.Minute)

	errCh := make(chan error, 1)
	go func() {
		slog.Info("服务已启动", "addr", cfg.Addr, "version", version, "data", cfg.DataDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("收到退出信号，正在关闭")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// warnOnFirstRun 在首次运行与公网监听时给出明确提示。
//
// 这个面板持有全部 Oracle 租户的完整控制权，暴露在公网上等同于把
// 所有云账号的钥匙挂在门口。默认只听回环，用户改成公网时必须知情。
func warnOnFirstRun(st *store.Store, cfg config.Config) error {
	count, err := st.CountUsers(context.Background())
	if err != nil {
		return err
	}
	if count == 0 {
		slog.Info("尚未初始化，请在浏览器中打开面板完成首次设置")
	}
	if cfg.ListensPublicly() {
		slog.Warn("正在监听非回环地址，本面板持有全部 Oracle 租户的控制权，" +
			"请务必置于 TLS 反向代理之后，并确认已启用两步验证")
	}
	// 旧开关的回落是「回环 + 私有网段」，私有网段内的主机仍可伪造来源 IP。
	// 这比旧语义（信任所有人）安全得多，但仍不是最优，提示一次让用户换掉。
	if len(cfg.TrustedProxies) > 0 && os.Getenv("OCICORE_TRUSTED_PROXIES") == "" &&
		os.Getenv("OCI_TOOLS_TRUSTED_PROXIES") == "" {
		slog.Warn("正在用 TRUST_PROXY 的兼容回落（信任回环与全部私有网段）。" +
			"建议改用 TRUSTED_PROXIES 显式声明代理网段，例如 127.0.0.0/8,10.0.0.0/8")
	}
	return nil
}

// ensureMasterKeyMayBeCreated 在「主密钥文件不存在」时判断能不能安全地新建一把。
//
// cryptobox.LoadOrCreateMasterKey 在文件缺失时会直接生成新密钥。对全新部署这是对的，
// 但对「密钥文件丢了」或「只从备份恢复了数据库」是灾难：库里所有密文会永久解不开，
// 而且新密钥立刻写回原路径，把用户还可能找回旧密钥的位置占掉。故障表现是
// 「升级后每个账号都报解密失败」——用户完全不知道该去找什么。
//
// 所以这里 fail-closed：库里确实有密文时拒绝启动，并给出两条明确出路。
// 确认库里没有要救的数据时，用 OCICORE_ALLOW_NEW_MASTER_KEY=true 显式放行。
func ensureMasterKeyMayBeCreated(cfg config.Config) error {
	if _, err := os.Stat(cfg.MasterKeyPath()); err == nil {
		return nil // 文件在，走正常读取路径
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("检查主密钥文件失败: %w", err)
	}

	sealed, err := store.HasSealedData(cfg.DBPath())
	if err != nil {
		return err
	}
	if !sealed {
		return nil
	}
	if cfg.AllowNewMasterKey {
		slog.Warn("主密钥文件缺失，而数据库中已有加密数据；"+
			"因 OCICORE_ALLOW_NEW_MASTER_KEY 已开启，仍将新建主密钥。原有密文将永久无法解密",
			"db", cfg.DBPath(), "key", cfg.MasterKeyPath())
		return nil
	}
	return fmt.Errorf(`主密钥文件缺失，但数据库中已有加密数据：%s
拒绝新建主密钥——新建会让库里所有私钥、TOTP 密钥、通知渠道凭据与代理密码永久无法解密。
请二选一：
  1) 把主密钥恢复到 %s（内容为 64 个十六进制字符），或用环境变量 OCICORE_MASTER_KEY 注入；
  2) 确认库里没有需要保留的数据后，设置 OCICORE_ALLOW_NEW_MASTER_KEY=true 重新启动。`,
		cfg.DBPath(), cfg.MasterKeyPath())
}

// buildHandler 把 API 与前端资源组合成一个处理器。
//
// 配置了 StaticDir 就用磁盘上的目录，否则用编译时嵌入的产物。
// 前者是给前端开发用的：改完 vite build 一下就能看到效果，不用重编 Go。
func buildHandler(api *httpapi.Server, cfg config.Config) (http.Handler, error) {
	var assets fs.FS
	if cfg.StaticDir != "" {
		slog.Info("使用磁盘上的前端资源", "dir", cfg.StaticDir)
		assets = os.DirFS(cfg.StaticDir)
	} else {
		embedded, err := web.FS()
		if err != nil {
			return nil, err
		}
		assets = embedded
	}
	fileServer := http.FileServer(http.FS(assets))

	mux := http.NewServeMux()
	mux.Handle("/api/", api.Handler())
	mux.Handle("/", spaHandler(assets, fileServer))
	return mux, nil
}

// spaHandler 提供静态资源，并把未匹配的路径回落到 index.html，
// 让前端路由（/accounts、/instances 等）在直接访问时也能正常渲染。
func spaHandler(assets fs.FS, fileServer http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(assets, path); err == nil {
			fileServer.ServeHTTP(w, r)
			return
		}
		// 未知路径一律交给 SPA 处理。带扩展名的请求例外——
		// 那是真的找不到资源，回落 HTML 只会让调试更困难。
		if strings.Contains(pathBase(path), ".") {
			http.NotFound(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		fileServer.ServeHTTP(w, r2)
	})
}

func pathBase(p string) string {
	if idx := strings.LastIndex(p, "/"); idx >= 0 {
		return p[idx+1:]
	}
	return p
}

// purgeSessionsPeriodically 定期清理过期会话，避免表无限增长。
func purgeSessionsPeriodically(ctx context.Context, st *store.Store) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := st.PurgeExpiredSessions(ctx); err != nil {
				slog.Warn("清理过期会话失败", "err", err)
			} else if n > 0 {
				slog.Info("已清理过期会话", "count", n)
			}
		}
	}
}
