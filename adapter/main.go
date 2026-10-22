// fastgpt-sync-adapter：多源数据连接器平台。
// 默认（无子命令）= daemon 模式：连接器拉数据 → NAS 归档（消息+附件原样落盘）；
// serve 子命令 = FastGPT API 文件库模式（读 NAS 提供 previewUrl，方案 B 已选定）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"fastgpt-sync-adapter/adapter/internal/auth"
	"fastgpt-sync-adapter/adapter/internal/classifier"
	"fastgpt-sync-adapter/adapter/internal/config"
	feishuconnector "fastgpt-sync-adapter/adapter/internal/connector/feishu"
	"fastgpt-sync-adapter/adapter/internal/connector/wecom"
	"fastgpt-sync-adapter/adapter/internal/engine"
	"fastgpt-sync-adapter/adapter/internal/fastgpt"
	"fastgpt-sync-adapter/adapter/internal/feishu"
	"fastgpt-sync-adapter/adapter/internal/identity"
	fastgmtsink "fastgpt-sync-adapter/adapter/internal/sink/fastgpt"
	nassink "fastgpt-sync-adapter/adapter/internal/sink/nas"
	"fastgpt-sync-adapter/adapter/internal/storage"
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
	log.Printf(" fastgpt-sync-adapter（连接器平台）")
	log.Printf(" 企微连接器 : enabled=%v", cfg.Connectors.WeCom.Enabled)
	log.Printf(" NAS Sink   : enabled=%v（归档：消息+附件原样落盘）", cfg.Sinks.NAS.Enabled)
	log.Printf(" 分类器     : enabled=%v（默认标签 %s）", cfg.Classifier.Enabled, cfg.Classifier.DefaultLabel)
	log.Printf("==============================================")

	eng := engine.New(cfg.Engine.StateFile)

	// ===== 存储层（SQLite：授权/身份/台账/去重）=====
	st, err := storage.Open(cfg.Storage.DBFile)
	if err != nil {
		log.Fatalf("存储层初始化失败: %v", err)
	}
	defer st.Close()
	log.Printf("[OK] SQLite 存储就绪（%s）", cfg.Storage.DBFile)

	// ===== 身份解析器（多维表格未到位前用本地映射兜底）=====
	ids := identity.NewLocalResolver(cfg.Identity.LocalFile, st)
	log.Printf("[OK] 身份解析器：source=%s（%s）", cfg.Identity.Source, cfg.Identity.LocalFile)

	// ===== Sink：NAS 归档 =====
	clf := classifier.New(cfg.Classifier)
	// NAS_URL 可覆盖 config.yaml（Docker 中通过 host.docker.internal 访问宿主机中继）。
	nasCfg := cfg.Sinks.NAS.Config
	if v := os.Getenv("NAS_URL"); v != "" {
		nasCfg.URL = v
		log.Printf("[NAS] 使用环境变量 NAS_URL=%s", v)
	}
	nas := nassink.New(nasCfg, clf, ids, st)
	if err := nas.Login(); err != nil {
		log.Fatalf("群晖 NAS 登录失败: %v", err)
	}
	log.Printf("[OK] 群晖 NAS 登录成功（root=%s）", cfg.Sinks.NAS.RootPath)
	eng.AddSink(nas)

	// 按 FastGPT 用户列表在 NAS 自动建目录（人工上传区；需账号级 api_key）
	if ud := cfg.Sinks.NAS.UserDirs; ud.Enabled {
		userClient := fastgpt.NewUserClient(ud.FastGPTBaseURL, ud.FastGPTAPIKey)
		if err := ensureUserDirs(nas, userClient, cfg.Sinks.NAS.RootPath); err != nil {
			log.Printf("[警告] 用户目录创建: %v", err)
		}
	}

	// ===== 连接器：企业微信 =====
	if cfg.Connectors.WeCom.Enabled {
		wc := wecom.New(cfg.Connectors.WeCom.WeCom)
		eng.AddConnector(engine.ConnectorJob{
			Conn:       wc,
			Interval:   cfg.Connectors.WeCom.Interval.D(),
			BatchLimit: cfg.Connectors.WeCom.BatchLimit,
			RunOnStart: cfg.Engine.RunOnStart,
		})
		log.Printf("[OK] 企微连接器已注册（间隔 %s，批 %d）", cfg.Connectors.WeCom.Interval.D(), cfg.Connectors.WeCom.BatchLimit)
	}

	// ===== 飞书客户端 + OAuth 授权 webhook =====
	fc := cfg.Connectors.Feishu
	var feishuClient *feishu.Client
	if fc.AppID != "" {
		feishuClient = feishu.New(fc.AppID, fc.AppSecret)
	}
	// ===== 连接器：飞书云文档 =====
	if fc.Enabled {
		fcConn := feishuconnector.New(feishuconnector.Config{
			AppID: fc.AppID, AppSecret: fc.AppSecret, Mode: fc.Mode, Folders: fc.Folders,
		}, st)
		eng.AddConnector(engine.ConnectorJob{
			Conn:       fcConn,
			Interval:   fc.Interval.D(),
			BatchLimit: fc.BatchLimit,
			RunOnStart: cfg.Engine.RunOnStart,
		})
		log.Printf("[OK] 飞书连接器已注册（mode=%s，共享文件夹 %d 个，间隔 %s）", fc.Mode, len(fc.Folders), fc.Interval.D())
	}
	if cfg.Auth.Enabled && feishuClient != nil {
		authSvc := auth.New(auth.Config{
			Addr:            cfg.Auth.Addr,
			ExternalURL:     cfg.Auth.ExternalURL,
			RefreshInterval: cfg.Auth.RefreshInterval.D(),
		}, feishuClient, st)
		// HTTP 服务单独 goroutine
		go func() {
			if err := authSvc.Start(); err != nil {
				log.Printf("[auth] webhook 退出: %v", err)
			}
		}()
		// token 刷新注册为引擎周期任务
		eng.AddTask(authSvc)
		log.Printf("[OK] 飞书授权 webhook 已启动（%s），token 刷新间隔 %s", cfg.Auth.Addr, cfg.Auth.RefreshInterval.D())
	}

	// ===== FastGPT 按人推送任务 =====
	if cfg.Sinks.FastGPT.Enabled {
		pushTask := fastgmtsink.New(fastgmtsink.Config{
			BaseURL:  cfg.Sinks.FastGPT.BaseURL,
			APIKey:   cfg.Sinks.FastGPT.APIKey,
			Interval: cfg.Sinks.FastGPT.Interval.D(),
		}, nas.Client(), cfg.Sinks.NAS.RootPath, st)
		eng.AddTask(pushTask)
		log.Printf("[OK] FastGPT 推送任务已注册（间隔 %s，%s）", cfg.Sinks.FastGPT.Interval.D(), cfg.Sinks.FastGPT.BaseURL)
	}

	if !cfg.Connectors.WeCom.Enabled && !cfg.Connectors.Feishu.Enabled {
		log.Printf("[提示] 未启用任何连接器，引擎空转（仅 Sink/授权 就绪）")
	}

	// 优雅退出
	ctx, cancel := context.WithCancel(context.Background())
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		log.Printf("收到退出信号，停止。")
		cancel()
	}()

	if err := eng.Run(ctx); err != nil {
		log.Fatalf("引擎运行失败: %v", err)
	}
}

// ensureUserDirs 拉取 FastGPT 用户列表，在 NAS 根目录下为每个用户建目录。
func ensureUserDirs(nas *nassink.Sink, userClient *fastgpt.UserClient, rootPath string) error {
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
		dir := rootPath + "/" + name
		if err := nas.Client().MkdirAll(dir); err != nil {
			log.Printf("  [建目录失败] %s: %v", dir, err)
			continue
		}
		log.Printf("  [建目录] %s", dir)
	}
	return nil
}
