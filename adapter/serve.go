package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"fastgpt-sync-adapter/adapter/internal/nasstore"
	"fastgpt-sync-adapter/adapter/internal/server"
	"fastgpt-sync-adapter/adapter/internal/store"
	"fastgpt-sync-adapter/adapter/internal/synology"
)

// serveMain serve 子命令：FastGPT API 文件库模式（方案 B）。
// 数据源二选一：--root 本地目录（PoC/调试），或 --nas-url 群晖 NAS（生产：FastGPT 经 previewUrl
// 直接拉到 NAS 上的归档文件，含 R6 冻结过滤与 .meta.json 隐藏）。
func serveMain(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", envOr("ADDR", ":8080"), "监听地址")
	root := fs.String("root", envOr("ROOT_DIR", ""), "本地目录模式：要暴露的目录")
	nasURL := fs.String("nas-url", envOr("NAS_URL", ""), "NAS 模式：群晖地址，如 https://nas.example.com:5001")
	nasUser := fs.String("nas-user", envOr("NAS_USER", ""), "NAS 模式：账号")
	nasPass := fs.String("nas-pass", envOr("NAS_PASS", ""), "NAS 模式：密码")
	nasRoot := fs.String("nas-root", envOr("NAS_ROOT", ""), "NAS 模式：根路径，如 /FastGPT-test")
	token := fs.String("token", envOr("AUTH_TOKEN", "dev-token"), "FastGPT 调用鉴权 token")
	externalURL := fs.String("external-url", envOr("EXTERNAL_URL", ""), "外部可访问地址（经隧道/反代时必填）")
	_ = fs.Parse(args)

	var backend server.Backend
	var sourceDesc string

	switch {
	case *nasURL != "":
		// NAS 模式
		if *nasUser == "" || *nasPass == "" || *nasRoot == "" {
			fmt.Fprintln(os.Stderr, "NAS 模式需要 --nas-user / --nas-pass / --nas-root")
			os.Exit(1)
		}
		nas := synology.New(*nasURL, *nasUser, *nasPass)
		if err := nas.Login(); err != nil {
			log.Fatalf("NAS 登录失败: %v", err)
		}
		backend = nasstore.New(nas, *nasRoot)
		sourceDesc = fmt.Sprintf("NAS %s %s", *nasURL, *nasRoot)
		log.Printf("[OK] NAS 登录成功")
	case *root != "":
		// 本地目录模式
		info, err := os.Stat(*root)
		if err != nil || !info.IsDir() {
			fmt.Fprintf(os.Stderr, "root 目录不可访问: %s\n", *root)
			os.Exit(1)
		}
		st, err := store.New(*root)
		if err != nil {
			log.Fatalf("store: %v", err)
		}
		backend = server.LocalBackend{Store: st}
		sourceDesc = "本地目录 " + *root
	default:
		fmt.Fprintln(os.Stderr, "serve 需要数据源：--root <本地目录> 或 --nas-url <群晖地址>")
		os.Exit(1)
	}

	srv := server.New(server.Options{
		Addr:        *addr,
		Token:       *token,
		ExternalURL: *externalURL,
	}, backend)

	log.Printf("==============================================")
	log.Printf(" fastgpt-sync-adapter [serve] API 文件库模式")
	log.Printf(" 监听地址   : %s", *addr)
	log.Printf(" 数据源     : %s", sourceDesc)
	log.Printf(" 外部地址   : %s", orDefault(*externalURL, "（按请求推断）"))
	log.Printf(" token      : %s", *token)
	log.Printf("==============================================")
	log.Printf(" FastGPT 知识库 → API 文件库 填：")
	log.Printf("   baseURL       = <外部地址或 http://<本机>:端口>")
	log.Printf("   authorization = %s", *token)
	log.Printf("==============================================")
	log.Printf(" 接口：POST /v1/file/list | GET /v1/file/content (previewUrl)")
	log.Printf("       GET  /v1/file/read | GET /v1/file/detail | GET /v1/file/download")

	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Fatalf("server: %v", err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
