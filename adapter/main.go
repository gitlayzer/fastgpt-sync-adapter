package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"fastgpt-sync-adapter/adapter/internal/config"
	"fastgpt-sync-adapter/adapter/internal/fastgpt"
	"fastgpt-sync-adapter/adapter/internal/syncer"
	"fastgpt-sync-adapter/adapter/internal/synology"
	"fastgpt-sync-adapter/adapter/internal/uploader"
	"fastgpt-sync-adapter/adapter/internal/wecom"
)

func main() {
	// serve 子命令：API 文件库模式（previewUrl 文件服务器）
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		serveMain(os.Args[2:])
		return
	}

	cfgPath := flag.String("config", "config.yaml", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("配置加载失败: %v", err)
	}

	log.Printf("==============================================")
	log.Printf(" fastgpt-sync-adapter（多源同步中间件）")
	log.Printf(" FastGPT    : %s (dataset=%s)", cfg.FastGPT.BaseURL, cfg.FastGPT.DatasetID)
	log.Printf(" 群晖 NAS   : %s (root=%s)", cfg.NAS.URL, cfg.NAS.RootPath)
	if cfg.WeCom.CorpID != "" {
		log.Printf(" 企业微信   : corpID=%s (会话存档)", cfg.WeCom.CorpID)
	} else {
		log.Printf(" 企业微信   : 未配置")
	}
	log.Printf(" 上传间隔   : %s", cfg.Sync.Interval)
	log.Printf("==============================================")

	// 1) FastGPT 上传客户端 + 用户客户端
	uploaderClient := uploader.NewClient(cfg.FastGPT.BaseURL, cfg.FastGPT.APIKey, cfg.FastGPT.DatasetID)
	uploaderClient.ImageIndex = cfg.FastGPT.ImageIndex
	uploaderClient.AutoIndexes = cfg.FastGPT.AutoIndexes
	uploaderClient.CustomPdfParse = cfg.FastGPT.CustomPdfParse
	userClient := fastgpt.NewUserClient(cfg.FastGPT.BaseURL, cfg.FastGPT.APIKey)

	// 2) 群晖 NAS 客户端
	nas := synology.New(cfg.NAS.URL, cfg.NAS.Username, cfg.NAS.Password)
	if err := nas.Login(); err != nil {
		log.Fatalf("群晖 NAS 登录失败: %v", err)
	}
	log.Printf("[OK] 群晖 NAS 登录成功")

	// 3) 按用户在 NAS 建目录
	if err := ensureUserDirs(nas, userClient, cfg); err != nil {
		log.Printf("[警告] 用户目录创建: %v", err)
	}

	// 4) 企微会话存档（若已配置且 SDK 可用）
	archiver := buildArchiver(cfg)

	// 5) NAS → FastGPT 上传同步器（监控 NAS 落盘目录）
	//    注意：syncer 需要一个可访问的本地路径。若 NAS 已挂载到本地，直接用挂载点；
	//    否则此处先用 NAS root 作为监控根（后续可改为先 rsync 回本地）。
	sync := syncer.New(cfg.NAS.RootPath, uploaderClient, cfg.Sync.StateFile)

	// 优雅退出
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// 启动即跑一轮
	if cfg.Sync.RunOnStart {
		runCycle(archiver, nas, sync, cfg)
	}

	ticker := time.NewTicker(cfg.Sync.Interval)
	defer ticker.Stop()
	log.Printf("进入定时轮询，每 %s 执行一轮（企微拉取→NAS落盘→上传FastGPT）。Ctrl+C 停止。", cfg.Sync.Interval)
	for {
		select {
		case <-ticker.C:
			runCycle(archiver, nas, sync, cfg)
		case <-stop:
			log.Printf("收到退出信号，停止。")
			return
		}
	}
}

// ensureUserDirs 拉取 FastGPT 用户列表，在 NAS 根目录下为每个用户建目录。
// 用户列表通过 API 自动获取；目录也随用户列表自动创建（新增用户自动补建）。
func ensureUserDirs(nas *synology.Client, userClient *fastgpt.UserClient, cfg *config.Config) error {
	users, err := userClient.ListUsers()
	if err != nil {
		return fmt.Errorf("获取 FastGPT 用户列表失败（api_key 需为账号级，能访问用户接口）: %w", err)
	}
	if len(users) == 0 {
		log.Printf("[提示] FastGPT 用户列表为空，跳过目录创建")
		return nil
	}
	for _, u := range users {
		name := u.Username
		if name == "" {
			name = u.Name
		}
		if name == "" {
			continue
		}
		dir := cfg.NAS.RootPath + "/" + name
		if err := nas.MkdirAll(dir); err != nil {
			log.Printf("  [建目录失败] %s: %v", dir, err)
			continue
		}
		log.Printf("  [建目录] %s", dir)
	}
	return nil
}

// buildArchiver 构建企微会话存档拉取器；SDK 不可用时返回 nil。
func buildArchiver(cfg *config.Config) *wecom.Archiver {
	if cfg.WeCom.CorpID == "" {
		return nil
	}
	pemText, err := cfg.WeComPrivateKeyPEM()
	if err != nil {
		log.Printf("[企微] 读取 RSA 私钥失败: %v", err)
		return nil
	}
	crypto, err := wecom.NewCrypto(pemText)
	if err != nil {
		log.Printf("[企微] RSA 私钥解析失败: %v", err)
		return nil
	}
	sdk, err := wecom.NewSDKFromLib(cfg.WeCom.CorpID, cfg.WeCom.Secret, cfg.WeCom.SDKLibPath)
	if err != nil {
		log.Printf("[企微] SDK 初始化失败（会话存档功能不可用）: %v", err)
		return nil
	}
	log.Printf("[OK] 企微会话存档已就绪")
	return wecom.NewArchiver(cfg.WeCom.CorpID, cfg.WeCom.Secret, crypto, sdk)
}

// runCycle 一轮完整流水线：企微拉取 → NAS 落盘 → 上传 FastGPT。
func runCycle(archiver *wecom.Archiver, nas *synology.Client, sync *syncer.Syncer, cfg *config.Config) {
	log.Printf("--- 开始同步 %s ---", time.Now().Format("2006-01-02 15:04:05"))

	// 1. 企微会话存档拉取（若可用）
	if archiver != nil {
		// TODO: 用持久化的 seq 游标增量拉取；落盘到 NAS 按用户/群目录
		log.Printf("[企微] 拉取会话存档…（SDK 已就绪）")
		// 示例骨架（待真实联调时启用）：
		// msgs, _, err := archiver.FetchAndDecrypt(lastSeq, 100)
		// for _, m := range msgs { 原样 JSON 落盘到 NAS 对应用户目录 }
	}

	// 2. NAS → FastGPT 增量上传
	if err := sync.Run(); err != nil {
		log.Printf("[上传] 出错: %v", err)
	}

	log.Printf("--- 本轮同步完成 ---")
}
