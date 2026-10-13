package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"fastgpt-sync-adapter/adapter/internal/server"
	"fastgpt-sync-adapter/adapter/internal/store"
)

// serveMain serve 子命令：FastGPT API 文件库模式。
// 把本地目录暴露为 FastGPT API 文件库，content 接口返回 previewUrl 外链，
// 由 FastGPT 直接下载原文件并自行解析。
func serveMain(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", envOr("ADDR", ":8080"), "监听地址")
	root := fs.String("root", envOr("ROOT_DIR", ""), "要暴露的本地目录")
	token := fs.String("token", envOr("AUTH_TOKEN", "dev-token"), "FastGPT 调用鉴权 token")
	externalURL := fs.String("external-url", envOr("EXTERNAL_URL", ""), "外部可访问地址（经隧道/反代时必填，如 https://xxx.trycloudflare.com）")
	_ = fs.Parse(args)

	if *root == "" {
		fmt.Fprintln(os.Stderr, "serve 模式需要 --root（或 ROOT_DIR）：要暴露的本地目录")
		os.Exit(1)
	}
	info, err := os.Stat(*root)
	if err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "root 目录不可访问: %s\n", *root)
		os.Exit(1)
	}

	st, err := store.New(*root)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	srv := server.New(server.Options{
		Addr:        *addr,
		Token:       *token,
		ExternalURL: *externalURL,
	}, st)

	log.Printf("==============================================")
	log.Printf(" fastgpt-sync-adapter [serve] API 文件库模式")
	log.Printf(" 监听地址   : %s", *addr)
	log.Printf(" 文件目录   : %s", *root)
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
