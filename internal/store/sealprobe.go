package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
)

// HasSealedData 在不解密、不建表的前提下，判断库里是否存在「必须有主密钥才能读出」的数据。
//
// 它服务于启动时的 fail-closed 判断：主密钥文件不见了、库里却还有密文时，
// 静默生成一把新密钥会让这些数据永久解不开（新密钥立刻写回原路径，还会把
// 用户可能仍能从备份找回旧密钥的位置占掉）。那种故障的表现是「升级后每个
// 账号都报解密失败」，用户完全不知道该去找什么，所以宁可拒绝启动。
//
// 只读地探测，不执行建表语句：探测本身不该改变被探测的对象——顺手建出一个
// 空库会让下一次判断被自己的副作用误导。
func HasSealedData(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil // 还没有库，等同于全新部署
		}
		return false, fmt.Errorf("store: 检查数据库文件失败: %w", err)
	}

	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return false, fmt.Errorf("store: 打开数据库失败: %w", err)
	}
	defer db.Close()

	ctx := context.Background()

	// 四类密文，覆盖所有用主密钥加密后落盘的数据：
	// 账号私钥（BLOB，建账号必有）、TOTP 密钥、通知渠道配置、代理密码。
	//
	// 只要有一处存在就说明「这台机器上有东西必须靠旧密钥才能读」，
	// 不必知道具体是哪一条。
	for _, q := range []string{
		`SELECT EXISTS(SELECT 1 FROM accounts WHERE key_ciphertext IS NOT NULL)`,
		`SELECT EXISTS(SELECT 1 FROM users WHERE totp_secret LIKE 'enc:v1:%')`,
		`SELECT EXISTS(SELECT 1 FROM notification_channels WHERE config LIKE 'enc:v1:%')`,
		`SELECT EXISTS(SELECT 1 FROM proxies WHERE pass_ciphertext IS NOT NULL)`,
	} {
		var found bool
		if err := db.QueryRowContext(ctx, q).Scan(&found); err != nil {
			// 表或列不存在（更老的库结构、或迁移尚未跑到）按「没有」处理：
			// 探测是为了保护数据，不该因为结构差异反过来挡住启动。
			if strings.Contains(err.Error(), "no such table") ||
				strings.Contains(err.Error(), "no such column") {
				continue
			}
			return false, fmt.Errorf("store: 探测加密数据失败: %w", err)
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}
