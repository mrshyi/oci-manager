// Package backup 负责整库的加密导出与恢复。
//
// 威胁模型与 cryptobox 一致但更强一层：备份文件会被拷到 U 盘、传到另一台
// 机器、扔进网盘。所以整份备份用口令派生的密钥做 AES-256-GCM 加密，
// 而不是「导出明文库再让用户自己加密」——后者一定会有人直接传明文。
//
// 容器格式（全部定长，便于定位与升级）：
//
//	MAGIC(8) | version(1) | salt(16) | nonce(12) | AES-256-GCM(ciphertext)
//
// 头部这四个字段同时作为 GCM 的附加认证数据（AAD）。这样改任何一个字节
// ——包括版本号——都会让认证失败，而不是让解密器按被篡改的版本去解析。
package backup

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/argon2"
)

// Magic 是备份文件的前 8 字节。用定长 ASCII 而不是 JSON 头：
// 读前 8 字节就能判断「这是不是一个备份文件」，不需要先解析。
const Magic = "OCICORE1"

// Version 是当前程序写出的备份格式版本。
//
// 恢复时拒绝**高于**本版本的文件：新版本可能用了本程序不认识的字段，
// 硬着头皮解开只会得到一份缺东西的库。低于等于本版本的可以恢复。
const Version = 1

const (
	saltSize  = 16
	nonceSize = 12
	keyLen    = 32

	// headerSize = MAGIC + version + salt + nonce
	headerSize = len(Magic) + 1 + saltSize + nonceSize
)

// argon2id 参数。
//
// t=3 / m=64MB / p=4 是 OWASP 给交互式登录口令的建议区间偏上沿：
// 派生一次约 0.2–0.5 秒，用户能接受，而暴力破解的代价被抬到很高。
// 备份文件里装着全部云账号的控制权，这个延迟值得。
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB，即 64 MB
	argonThreads = 4
)

var (
	// ErrWrongPassword 同时覆盖「口令错」与「文件被改过一个字节」。
	//
	// 两者在密码学上不可区分（GCM 认证失败就是失败），也不该区分：
	// 告诉攻击者「口令对了但文件坏了」等于确认了口令正确。
	ErrWrongPassword = errors.New("backup: 口令不正确，或备份文件已损坏")

	// ErrNotBackup 表示这不是本工具生成的备份。
	ErrNotBackup = errors.New("backup: 不是本工具生成的备份文件")

	// ErrVersionTooNew 表示备份来自更新的版本。
	ErrVersionTooNew = errors.New("backup: 备份格式版本高于当前程序，请先升级")
)

// Header 是解密前的文件头。它本身不加密，但受 GCM 的 AAD 保护。
type Header struct {
	Version int
	Salt    []byte
	Nonce   []byte
}

// DeriveKey 用 argon2id 从口令派生密钥。
//
// 单独导出是为了让测试能验证「同样的口令 + 同样的 salt 得到同样的密钥」，
// 也是为了让参数只有一处定义。
func DeriveKey(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, keyLen)
}

// Seal 把明文封装成备份容器。
func Seal(password string, plaintext []byte) ([]byte, error) {
	if password == "" {
		return nil, errors.New("backup: 必须设置口令")
	}
	salt := make([]byte, saltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("backup: 生成 salt 失败: %w", err)
	}
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("backup: 生成 nonce 失败: %w", err)
	}

	header := buildHeader(salt, nonce)
	key := DeriveKey(password, salt)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("backup: 初始化分组密码失败: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("backup: 初始化 GCM 失败: %w", err)
	}

	out := make([]byte, 0, headerSize+len(plaintext)+aead.Overhead())
	out = append(out, header...)
	out = aead.Seal(out, nonce, plaintext, header)
	return out, nil
}

// Open 解开备份容器。口令错与文件被改都返回 ErrWrongPassword。
func Open(password string, data []byte) ([]byte, error) {
	h, err := ParseHeader(data)
	if err != nil {
		return nil, err
	}
	key := DeriveKey(password, h.Salt)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("backup: 初始化分组密码失败: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("backup: 初始化 GCM 失败: %w", err)
	}

	header := data[:headerSize]
	plaintext, err := aead.Open(nil, h.Nonce, data[headerSize:], header)
	if err != nil {
		return nil, ErrWrongPassword
	}
	return plaintext, nil
}

// ParseHeader 解析文件头。不派生密钥，所以很快——
// 版本不兼容时不必先花半秒算 argon2。
func ParseHeader(data []byte) (Header, error) {
	if len(data) < headerSize {
		return Header{}, ErrNotBackup
	}
	if string(data[:len(Magic)]) != Magic {
		return Header{}, ErrNotBackup
	}
	version := int(data[len(Magic)])
	if version > Version {
		return Header{}, ErrVersionTooNew
	}
	if version < 1 {
		return Header{}, ErrNotBackup
	}
	off := len(Magic) + 1
	return Header{
		Version: version,
		Salt:    data[off : off+saltSize],
		Nonce:   data[off+saltSize : off+saltSize+nonceSize],
	}, nil
}

// buildHeader 组装固定布局的头部。版本号写在这里，同时进 AAD。
func buildHeader(salt, nonce []byte) []byte {
	out := make([]byte, 0, headerSize)
	out = append(out, Magic...)
	out = append(out, byte(Version))
	out = append(out, salt...)
	out = append(out, nonce...)
	return out
}

// SHA256 是给清单用的短摘要，不是安全边界。
func SHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:])
}
