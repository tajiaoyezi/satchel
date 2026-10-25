package agentconn

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/satchel/satchel/internal/base/db"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// LoadIdentity 读数据目录下的 master.key（主控的 Ed25519 身份，原始 32 字节种子，node-channel「主控的通信身份」）；
// 文件不存在时生成一把并以 0600 写入。文件存在但不是 32 字节时返回 config 错误、不改写它：
// 换了身份所有已配对的节点都会连不上，所以不像 mmwx 那样悄悄重新生成。
func LoadIdentity(dataDir string) (ed25519.PrivateKey, error) {
	path := filepath.Join(dataDir, db.MasterKeyFile)
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(data) != ed25519.SeedSize {
			return nil, v1.Newf(v1.CodeConfig, "主控通信密钥 %s 不合法：应当是 %d 字节，实际 %d 字节", path, ed25519.SeedSize, len(data)).
				WithNext("从备份里找回原来的 master.key 放回去；确认要换一把新身份时删掉这个文件再启动，已配对的节点都要重新配对")
		}
		return ed25519.NewKeyFromSeed(data), nil
	case errors.Is(err, fs.ErrNotExist):
	default:
		return nil, v1.Wrap(v1.CodeConfig, "读取主控通信密钥 "+path+" 失败", err)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "生成主控通信密钥失败", err)
	}
	// 先写临时文件再改名：写到一半断电不会留下一个长度不对的 master.key。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, priv.Seed(), 0o600); err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "写主控通信密钥失败", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, v1.Wrap(v1.CodeInternal, "写主控通信密钥失败", err)
	}
	return priv, nil
}
