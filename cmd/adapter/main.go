// fastgpt-sync-adapter：多源数据连接器平台。
// 默认（无子命令）= daemon 模式：连接器拉数据 → NAS 归档（消息+附件原样落盘）；
// serve 子命令 = FastGPT API 文件库模式（读 NAS 提供 previewUrl，方案 B 已选定）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gitlayzer/fastgpt-sync-adapter/internal/auth"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/classifier"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/config"
	feishuconnector "github.com/gitlayzer/fastgpt-sync-adapter/internal/connector/feishu"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/connector/wecom"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/engine"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/fastgpt"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/feishu"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/identity"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/nasstore"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/server"
	fastgmtsink "github.com/gitlayzer/fastgpt-sync-adapter/internal/sink/fastgpt"
	nassink "github.com/gitlayzer/fastgpt-sync-adapter/internal/sink/nas"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/storage"
	"github.com/gitlayzer/fastgpt-sync-adapter/internal/version"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "-v", "--version":
			fmt.Println(version.Info())
			return
		case "serve":
			serveMain(os.Args[2:])
			return
		}
	}

	cfgPath := flag.String("config", "config.yaml", "配置文件路径")
	showVersion := flag.Bool("version", false, "显示版本信息并退出")
	flag.BoolVar(showVersion, "v", false, "显示版本信息并退出 (简写)")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Info())
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("配置加载失败: %v", err)
	}

	log.Printf("==============================================")
	log.Printf(" %s", version.Info())
	log.Printf(" 企微连接器 : enabled=%v", cfg.Connectors.WeCom.Enabled)
	log.Printf(" 飞书连接器 : enabled=%v", cfg.Connectors.Feishu.Enabled)
	log.Printf(" NAS Sink   : enabled=%v（归档：消息+附件原样落盘）", cfg.Sinks.NAS.Enabled)
	log.Printf(" API 文件库 : enabled=%v（监听 %s，token=%s）", cfg.Server.Enabled, cfg.Server.Addr, cfg.Server.Token)
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

	// ===== 身份解析器 =====
	var ids identity.Resolver
	switch cfg.Identity.Source {
	case "bitable":
		bt := cfg.Identity.Bitable
		bc := feishu.New(bt.AppID, bt.AppSecret)
		br := identity.NewBitableResolver(identity.BitableConfig{
			AppToken:        bt.AppToken,
			TableID:         bt.TableID,
			Refresh:         bt.RefreshInterval.D(),
			FieldWeComID:    bt.FieldWeComID,
			FieldWeComName:  bt.FieldWeComName,
			FieldFeishuUser: bt.FieldFeishuUser,
			FieldOpenID:     bt.FieldOpenID,
		}, bc, st)
		// 启动时先同步一次，避免首轮归档用空缓存把目录建成 userid
		if err := br.Sync(); err != nil {
			log.Printf("[警告] 多维表格首次同步失败（将按间隔重试）: %v", err)
		}
		ids = br
		log.Printf("[OK] 身份解析器：source=bitable（表格 %s/%s，间隔 %s）",
			bt.AppToken, bt.TableID, bt.RefreshInterval.D())
	default:
		ids = identity.NewLocalResolver(cfg.Identity.LocalFile, st)
		log.Printf("[OK] 身份解析器：source=local（%s）", cfg.Identity.LocalFile)
	}

	// ===== Sink：NAS 归档 =====
	clf := classifier.New(cfg.Classifier)
	// NAS_URL 可覆盖 config.yaml（Docker 中通过 host.docker.internal 访问宿主机中继）。
		nasCfg := cfg.Sinks.NAS.Config
		nasCfg.GroupNames = cfg.Connectors.WeCom.GroupNames
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
			WikiSpaceIDs: fc.WikiSpaceIDs, CollabRefresh: fc.CollabRefresh.D(),
			SharedDiscover: fc.SharedDiscover.D(),
			SearchKeys: fc.SearchKeys,
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
			// 启动前先同步刷新一次快过期的授权，避免首轮飞书扫描遇到已过期 token 竞争
			_ = authSvc.Run(context.Background())
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

	// ===== FastGPT API 文件库 HTTP 服务（serve 模式，随 daemon 同步启动） =====
	var apiServer *http.Server
	if cfg.Server.Enabled && cfg.Sinks.NAS.Enabled {
		backend := nasstore.New(nas.Client(), cfg.Sinks.NAS.RootPath)
		srv := server.New(server.Options{
			Addr:        cfg.Server.Addr,
			Token:       cfg.Server.Token,
			ExternalURL: cfg.Server.ExternalURL,
		}, backend)
		apiServer = &http.Server{
			Addr:    cfg.Server.Addr,
			Handler: srv.Handler(),
		}
		go func() {
			log.Printf("[OK] FastGPT API 文件库服务已启动（%s，token=%s）", cfg.Server.Addr, cfg.Server.Token)
			if err := apiServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("[server] API 文件库退出: %v", err)
			}
		}()
	}

	// 优雅退出
	ctx, cancel := context.WithCancel(context.Background())
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		log.Printf("收到退出信号，正在停止（再按一次 Ctrl+C 强制退出）…")
		cancel()

		// 兜底：SDK 的 C 调用是同步阻塞的，单个 GetMediaData 最长要等到
		// mediaTimeoutSec 才返回，卡在最坏路径上时优雅退出仍会拖一会。
		// 第二次信号立即强退；否则 15 秒后也强退，避免用户以为进程卡死。
		select {
		case <-stop:
			log.Printf("再次收到中断信号，强制退出")
			os.Exit(130)
		case <-time.After(15 * time.Second):
			log.Printf("优雅退出超时（15s），强制退出")
			os.Exit(130)
		}
	}()

	err = eng.Run(ctx)
	if apiServer != nil {
		shutdownCtx, sCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer sCancel()
		_ = apiServer.Shutdown(shutdownCtx)
	}
	if err != nil {
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
