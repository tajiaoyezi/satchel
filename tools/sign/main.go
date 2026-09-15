// sign 是发布签名程序：用 Ed25519 私钥给发布物做分离签名，产出 <file>.sig（原始 64 字节）。
//
//	go run ./tools/sign keygen                 # 生成一对密钥，base64 打印私钥与公钥
//	RELEASE_SIGNING_PRIVATE_KEY=<base64> go run ./tools/sign sign <file>...
//	go run ./tools/sign verify <file> <sig>    # 用 pkg/release 里的公钥清单验回
//
// 私钥只从环境变量 RELEASE_SIGNING_PRIVATE_KEY 读（base64 的 64 字节 Ed25519 私钥）：
// CI 由受保护环境 release-signing 的 secret 注入；本地由不进仓库的脚本注入。不接受命令行传私钥——
// 命令行会进 shell 历史与进程列表。三个二进制共用这一把，agent 与测速端的发布线检出本仓库来跑它。
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"

	"github.com/satchel/satchel/pkg/release"
)

// EnvPrivateKey 是私钥所在的环境变量。
const EnvPrivateKey = "RELEASE_SIGNING_PRIVATE_KEY"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv(EnvPrivateKey)))
}

func run(args []string, stdout, stderr *os.File, privB64 string) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "keygen":
		if len(args) != 1 {
			usage(stderr)
			return 2
		}
		return keygen(stdout, stderr)
	case "sign":
		if len(args) < 2 {
			usage(stderr)
			return 2
		}
		return sign(args[1:], stdout, stderr, privB64)
	case "verify":
		if len(args) != 3 {
			usage(stderr)
			return 2
		}
		if err := release.VerifyFile(args[1], args[2]); err != nil {
			fmt.Fprintf(stderr, "验签失败 %s：%v\n", args[1], err)
			return 1
		}
		fmt.Fprintf(stdout, "验签通过 %s\n", args[1])
		return 0
	}
	usage(stderr)
	return 2
}

func usage(w *os.File) {
	fmt.Fprintln(w, "用法：sign keygen | sign sign <file>... | sign verify <file> <sig>")
	fmt.Fprintln(w, "私钥只从环境变量 "+EnvPrivateKey+" 读（base64 的 64 字节 Ed25519 私钥）")
}

func keygen(stdout, stderr *os.File) int {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintln(stderr, "生成密钥失败：", err)
		return 1
	}
	fmt.Fprintf(stdout, "private (%s):\n%s\n", EnvPrivateKey, base64.StdEncoding.EncodeToString(priv))
	fmt.Fprintf(stdout, "public (pkg/release.PublicKeys):\n%s\n", base64.StdEncoding.EncodeToString(pub))
	return 0
}

func sign(files []string, stdout, stderr *os.File, privB64 string) int {
	if privB64 == "" {
		fmt.Fprintln(stderr, "环境变量 "+EnvPrivateKey+" 未设置，无法签名")
		return 1
	}
	raw, err := base64.StdEncoding.DecodeString(privB64)
	if err != nil || len(raw) != ed25519.PrivateKeySize {
		fmt.Fprintf(stderr, "私钥不合法：需要 base64 的 %d 字节 Ed25519 私钥\n", ed25519.PrivateKeySize)
		return 1
	}
	priv := ed25519.PrivateKey(raw)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintf(stderr, "读取 %s 失败：%v\n", f, err)
			return 1
		}
		sig := ed25519.Sign(priv, data)
		if err := os.WriteFile(f+".sig", sig, 0o644); err != nil {
			fmt.Fprintf(stderr, "写 %s.sig 失败：%v\n", f, err)
			return 1
		}
		fmt.Fprintf(stdout, "已签名 %s -> %s.sig\n", f, f)
	}
	return 0
}
