// main.go 网关入口：加载配置、装配账号池 / 上游客户端 / 调度器 / 面板，
// 起 HTTP 服务并处理优雅退出。
//
// 装配顺序有讲究：
//  1. 先加载账号（池需要知道有哪些号才能恢复状态）；
//  2. 再恢复池状态（冷却/统计），与目录对齐（增删账号）；
//  3. 面板要在 server 之前建（server 的路由要把 /panel/ 挂进去）；
//  4. 调度器最后起（它依赖池与上游都就绪）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rsfree/xhx2api/internal/auth"
	"github.com/rsfree/xhx2api/internal/cosx"
	"github.com/rsfree/xhx2api/internal/livecfg"
	"github.com/rsfree/xhx2api/internal/panel"
	"github.com/rsfree/xhx2api/internal/pool"
	"github.com/rsfree/xhx2api/internal/prompt"
	"github.com/rsfree/xhx2api/internal/reqlog"
	"github.com/rsfree/xhx2api/internal/scheduler"
	"github.com/rsfree/xhx2api/internal/server"
	"github.com/rsfree/xhx2api/internal/session"
	"github.com/rsfree/xhx2api/internal/upstream"
	"github.com/rsfree/xhx2api/internal/usage"
	"github.com/rsfree/xhx2api/internal/version"
)

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径（不存在时自动生成推荐配置）")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// 首次运行：自动落一份推荐配置（含随机 api_key）再加载。
			if key, werr := WriteDefault(*cfgPath); werr == nil {
				log.Printf("配置 %s 不存在，已生成推荐配置（api_key=%s，记录在该文件里，可自行修改）", *cfgPath, key)
				cfg, err = Load(*cfgPath)
			}
		}
		if err != nil {
			log.Fatalf("加载配置失败: %v", err)
		}
	}
	log.Printf("生效配置：监听=%s 账号目录=%s 密钥=%s 策略=%s", cfg.Listen, cfg.AuthDir, maskKey(cfg.APIKey), cfg.Pool.Strategy)

	// 账号：单文件损坏只警告，不阻断启动（一个手抖改坏的 JSON 不该让整池起不来）。
	auths, aerr := auth.LoadDir(cfg.AuthDir)
	if aerr != nil {
		log.Printf("WARN: 账号目录有文件无法解析: %v", aerr)
	}
	log.Printf("已加载 %d 个账号（%s）", len(auths), cfg.AuthDir)

	// 上游客户端。
	up := upstream.New()
	up.SetAgentHosts(cfg.Upstream.LLMBase, cfg.Upstream.AuthBase)
	up.SetAPIHosts(cfg.Upstream.PointsBase, cfg.Upstream.Origin)
	up.SetTimeouts(cfg.upstreamShort, cfg.upstreamHeader, cfg.upstreamIdle)
	up.SetIdentity(cfg.Upstream.UserAgent, cfg.Upstream.ClientVersion)
	up.SetProxy(proxyConfigOf(cfg))
	// 续期后必须落盘：新 refresh_token 只在内存里，重启后账号就掉线了。
	up.SetPersistHook(func(a *auth.Auth) error {
		if err := a.SaveAtomic(); err != nil {
			log.Printf("WARN: 账号 %s 续期后落盘失败（重启后需重新登录）: %v", a.UID, err)
			return err
		}
		return nil
	})
	up.SetCatalogFile(cfg.Upstream.CatalogCacheFile)
	_ = os.MkdirAll(filepath.Dir(cfg.Upstream.CatalogCacheFile), 0o700)
	if err := up.LoadCatalogFile(cfg.Upstream.CatalogCacheFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("WARN: 恢复模型目录缓存失败: %v", err)
	}

	// 账号池。
	p := pool.New(pool.Config{
		Strategy:      pool.Strategy(cfg.Pool.Strategy),
		MaxInFlight:   cfg.Pool.MaxInFlight,
		BreakerThresh: cfg.Pool.BreakerThreshold,
		BreakerCool:   cfg.breakerCooldown,
		BreakerMax:    cfg.breakerMax,
		DegradeThresh: cfg.Pool.DegradeThreshold,
		DegradeCool:   cfg.degradeCooldown,
		SoftRateBase:  cfg.softRateDur,
		SoftRateMax:   cfg.softRateMaxDur,
		PlanCool:      cfg.planCreditDur,
		IdleWeightPer: time.Duration(float64(time.Hour) / cfg.Pool.IdleWeightPerHour),
		IdleWeightMax: cfg.Pool.IdleWeightMax,
		StateFile:     cfg.StateFile,
	})
	// 先同步目录再恢复状态：恢复时按 uid 找账号，账号不在池里的状态会被丢弃。
	if added, removed := p.SyncToDir(auths); added > 0 || removed > 0 {
		log.Printf("账号池与目录对齐：新增 %d，移除 %d", added, removed)
	}
	if restored, rerr := p.Restore(cfg.StateFile); rerr != nil {
		log.Printf("WARN: 恢复池状态失败（按全新状态启动）: %v", rerr)
	} else if restored > 0 {
		log.Printf("已恢复 %d 个账号的池状态", restored)
	}
	p.SyncToDir(auths) // Restore 可能补了 uid，再对齐一次

	// 打印每个账号的「来源 + 出口 IP」。
	//
	// 放在池就绪之后：出口是按账号算的，得先知道有哪些账号。这一步也是
	// 验证「粘性真的生效」的唯一手段——如果 11 个号打出来只有 1-2 个出口，
	// 说明代理用户名没带上账号，分散等于没做。
	if up.ProxyEnabled() {
		probeCtx, cancelProbe := context.WithTimeout(context.Background(), 90*time.Second)
		go func() {
			defer cancelProbe()
			up.LogProxyEgress(probeCtx, p.UIDs())
		}()
	}

	flushCtx, stopFlush := context.WithCancel(context.Background())
	defer stopFlush()
	p.StartFlusher(flushCtx, 30*time.Second)

	// 会话粘性。
	var sessRouter *session.Router
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{TTL: cfg.sessionTTL, GCInterval: cfg.sessionGC})
		defer sessRouter.Close()
	}

	// 可观测。
	usagePath := stateSibling(cfg.StateFile, "usage.json")
	rec := usage.New(usagePath)
	rec.StartFlusher(flushCtx.Done(), time.Minute)
	archiveDir := stateSibling(cfg.StateFile, "requests")
	requestLog := reqlog.New(reqlog.Config{
		RingSize: 500, ArchiveDir: archiveDir, RetentionDays: cfg.Logging.RequestRetentionDays,
		MaxMB: cfg.Logging.RequestArchiveMaxMB, ClientInfo: cfg.Logging.RequestClientInfo,
	})
	defer requestLog.Close()
	if !cfg.Logging.RequestArchiveEnabled {
		// 关闭归档：把目录清空后置空，避免继续写盘。
		requestLog = reqlog.New(reqlog.Config{RingSize: 500, ClientInfo: cfg.Logging.RequestClientInfo})
	}

	// 提示词。
	promptText, perr := prompt.LoadText(cfg.Prompt.File)
	if perr != nil {
		log.Printf("WARN: 读取提示词文件失败（回退 passthrough）: %v", perr)
		cfg.Prompt.Mode = "passthrough"
	}

	// 热配置：面板保存后立即生效的那部分。
	live := livecfg.New(livecfg.Snapshot{
		APIKey:           cfg.APIKey,
		SoftCooldown:     int64(cfg.softRateDur),
		HiddenModels:     livecfg.NormalizeHiddenModels(cfg.Models.Hidden),
		RecordClientInfo: cfg.Logging.RequestClientInfo,
		PromptMode:       prompt.Normalize(cfg.Prompt.Mode),
		PromptText:       promptText,
		Aliases:          cfg.Aliases,
		DefaultModel:     cfg.DefaultModel,
		ImageAliases:     cfg.Images.Aliases,
	})

	// 调度器（只依赖池与上游，先建好再交给面板）。
	sch := scheduler.New(p, up, scheduler.Config{
		CheckinHours: cfg.Schedule.CheckinHours, KeepaliveHours: cfg.Schedule.KeepaliveHours,
		CheckinEnabled: cfg.Schedule.CheckinEnabled, KeepaliveEnabled: cfg.Schedule.KeepaliveEnabled,
		BalanceRefreshEnabled: cfg.Schedule.BalanceRefreshEnabled,
		BalanceRefreshMinutes: cfg.Schedule.BalanceRefreshMinutes,
		IncludeDisabled:       cfg.Schedule.IncludeDisabledInTasks,
		NewAccountGraceDays:   cfg.Schedule.NewAccountGraceDays, Concurrency: cfg.Schedule.Concurrency,
	})

	// 面板。
	panelStarted := time.Now()
	pn := panel.New(panel.Config{
		Pool: p, Upstream: up, Scheduler: sch,
		AuthDir: cfg.AuthDir, APIKey: cfg.APIKey, Live: live, Version: version.String(),
		ConfigPath: *cfgPath,
		LoadConfig: func() (any, error) {
			cur, err := Load(*cfgPath)
			if err != nil {
				return nil, err
			}
			return cur, nil
		},
		SaveConfig: func(raw []byte, replace bool) ([]string, error) {
			save := SaveConfig
			if replace {
				save = ReplaceConfig
			}
			next, restart, err := save(*cfgPath, raw)
			if err != nil {
				return nil, err
			}
			applyHotConfig(next, live, p, up, rec, requestLog, sch)
			return restart, nil
		},
		Usage: rec, RequestLog: requestLog,
		SessionStats: func() session.Stats {
			if sessRouter == nil {
				return session.Stats{Enabled: false}
			}
			return sessRouter.Stats()
		},
		CallbackBase: resolveCallbackBase(cfg),
		AppName:      cfg.Login.AppName,
		ClientDirs:   cfg.Import.ClientDirs,
		DefaultModel: cfg.DefaultModel, Aliases: cfg.Aliases,
		Logs:      panel.NewRing(500),
		StartedAt: panelStarted,
	})
	log.SetOutput(io.MultiWriter(os.Stderr, pn.Logs()))

	// 图片存储：配置了 COS 就优先走对象存储，上传失败会自动回落本地盘。
	var imageUploader server.ImageUploader
	if cfg.Images.COS.Enabled {
		cs, cerr := cosx.New(cosx.Config{
			SecretID:   cfg.Images.COS.SecretID,
			SecretKey:  cfg.Images.COS.SecretKey,
			Bucket:     cfg.Images.COS.Bucket,
			Region:     cfg.Images.COS.Region,
			Prefix:     cfg.Images.COS.Prefix,
			BaseURL:    cfg.Images.COS.BaseURL,
			PublicRead: cfg.Images.COS.PublicRead,
			SignedTTL:  time.Duration(cfg.Images.COS.SignedTTLSeconds) * time.Second,
		})
		if cerr != nil {
			log.Fatalf("images.cos 配置有误: %v", cerr)
		}
		imageUploader = cs
		log.Printf("图片存储：腾讯云 COS bucket=%s region=%s 前缀=%q",
			cfg.Images.COS.Bucket, cfg.Images.COS.Region, cfg.Images.COS.Prefix)
	}

	// 主 handler。
	h := server.NewHandler(server.Config{
		Pool: p, Upstream: up, APIKey: cfg.APIKey, Live: live,
		Session: sessRouter, StickyByPrefix: cfg.SessionSticky.PrefixAffinity,
		Usage: rec, RequestLog: requestLog, Panel: pn,
		DefaultModel: cfg.DefaultModel, Aliases: cfg.Aliases,
		PromptMode: prompt.Normalize(cfg.Prompt.Mode), PromptText: promptText,
		Version:               version.String(),
		MinOutputTokens:       cfg.Server.MinOutputTokens,
		ExcludeUpstreamHidden: cfg.Models.IncludeUpstreamHidden != nil && !*cfg.Models.IncludeUpstreamHidden,
		DisableImages:         cfg.Images.Enabled != nil && !*cfg.Images.Enabled,
		ImageTimeout:          time.Duration(cfg.Images.TimeoutSeconds) * time.Second,
		ImageReturnURL:        cfg.ImageURLEnabled(),
		ImageDefaultFormat:    cfg.ImageDefaultFormat(),
		ImageMaxTotalMB:       cfg.ImageMaxTotalMB(),
		ImagePublicBase:       cfg.Images.PublicBase,
		ImageDir:              imageDirOf(cfg),
		ImageUploader:         imageUploader,
		QueueWait:             cfg.queueWait,
		RefreshSkew:           time.Duration(cfg.Server.RefreshSkewSeconds) * time.Second,
		// URL 垫图：开关翻成「禁用」（handler 零值 = 允许），预算与白名单原样传。
		ImageGenerationsAcceptsImage: cfg.Images.GenerationsAcceptsImage,
		DisableImageURLInput:         !cfg.ImageURLInputEnabled(),
		ImageURLFetchTimeout:         cfg.ImageURLFetchTimeout(),
		ImageURLFetchAllowHosts:      cfg.Images.URLFetchAllowHosts,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)
	// 无条件启动：开关在循环内部判，面板改开关才能热生效。
	go sch.StartBalanceRefresh(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       cfg.readTimeout,
		// 不设全局 WriteTimeout：长流式生成合法时长可达数分钟，全局写超时会误杀
		// 在途 SSE。连接泄漏由 IdleTimeout 兜底（流式期间连接非空闲，不受影响）。
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		log.Printf("收到退出信号，正在落盘并停机…")
		_ = p.Flush()
		_ = rec.Flush()
		stopFlush()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.ShutdownTimeoutSeconds)*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("xhx2api %s 已启动：监听 %s，管理面板 http://127.0.0.1%s/panel/", version.String(), cfg.Listen, portOf(cfg.Listen))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("HTTP 服务异常退出: %v", err)
	}
	log.Printf("已退出")
}

// applyHotConfig 把能热改的字段应用到运行中的对象。
func applyHotConfig(c *Config, live *livecfg.Holder, p *pool.Pool, up *upstream.Client,
	rec *usage.Recorder, requestLog *reqlog.Recorder, sch *scheduler.Scheduler) {
	text, _ := prompt.LoadText(c.Prompt.File)
	live.Store(livecfg.Snapshot{
		APIKey: c.APIKey, SoftCooldown: int64(c.softRateDur),
		HiddenModels:     livecfg.NormalizeHiddenModels(c.Models.Hidden),
		RecordClientInfo: c.Logging.RequestClientInfo,
		PromptMode:       prompt.Normalize(c.Prompt.Mode), PromptText: text,
		Aliases: c.Aliases, DefaultModel: c.DefaultModel,
		ImageAliases: c.Images.Aliases,
	})
	strategy, err := pool.ParseStrategy(c.Pool.Strategy)
	if err != nil {
		strategy = pool.StrategyRoundRobin
	}
	p.Configure(func(pc *pool.Config) {
		pc.Strategy = strategy
		pc.MaxInFlight = c.Pool.MaxInFlight
		pc.BreakerThresh = c.Pool.BreakerThreshold
		pc.BreakerCool = c.breakerCooldown
		pc.BreakerMax = c.breakerMax
		pc.DegradeThresh = c.Pool.DegradeThreshold
		pc.DegradeCool = c.degradeCooldown
		pc.SoftRateBase = c.softRateDur
		pc.SoftRateMax = c.softRateMaxDur
		pc.PlanCool = c.planCreditDur
		pc.IdleWeightPer = time.Duration(float64(time.Hour) / c.Pool.IdleWeightPerHour)
		pc.IdleWeightMax = c.Pool.IdleWeightMax
	})
	up.SetIdentity(c.Upstream.UserAgent, c.Upstream.ClientVersion)
	up.SetProxy(proxyConfigOf(c))
	// 调度参数也热更新：以前这里完全不碰 scheduler，面板上改签到时点/并发/
	// 余额周期全部「保存成功但不生效」，要重启才看得出效果——用户以为配置坏了。
	if sch != nil {
		sch.Configure(func(sc *scheduler.Config) {
			sc.CheckinEnabled = c.Schedule.CheckinEnabled
			sc.KeepaliveEnabled = c.Schedule.KeepaliveEnabled
			sc.BalanceRefreshEnabled = c.Schedule.BalanceRefreshEnabled
			// 小时列表为空时保留原值：空列表会被 normalize 当成「没配置」补默认，
			// 那等于把用户刚清空的设置又偷偷塞回去。
			if len(c.Schedule.CheckinHours) > 0 {
				sc.CheckinHours = c.Schedule.CheckinHours
			}
			if len(c.Schedule.KeepaliveHours) > 0 {
				sc.KeepaliveHours = c.Schedule.KeepaliveHours
			}
			if c.Schedule.BalanceRefreshMinutes > 0 {
				sc.BalanceRefreshMinutes = c.Schedule.BalanceRefreshMinutes
			}
			sc.IncludeDisabled = c.Schedule.IncludeDisabledInTasks
			sc.NewAccountGraceDays = c.Schedule.NewAccountGraceDays
			if c.Schedule.Concurrency > 0 {
				sc.Concurrency = c.Schedule.Concurrency
			}
		})
	}
	log.Printf("配置已热更新：密钥=%s 策略=%s 软冷却=%s", maskKey(c.APIKey), strategy, c.Cooldown.SoftRate)
}

// proxyConfigOf 把配置映射成上游客户端的代理参数。
//
// 单独一个函数是为了让「启动装配」和「热更新」用同一份映射：两边各写一遍
// 字段必然漂移，而漂移的表现是「改了配置重启才生效」——正是刚修过的坑。
func proxyConfigOf(c *Config) upstream.ProxyConfig {
	p := c.Upstream.Proxy
	return upstream.ProxyConfig{
		Enabled:        p.Enabled,
		Template:       p.Template,
		DialTimeout:    time.Duration(p.DialTimeoutSeconds) * time.Second,
		EgressCheckURL: p.EgressCheckURL,
		EgressTTL:      time.Duration(p.EgressTTLMinutes) * time.Minute,
		Scope:          p.Scope,
	}
}

// imageDirOf 决定图片落盘目录：配置优先，否则放在 state 文件旁边的 images/。
func imageDirOf(c *Config) string {
	if v := strings.TrimSpace(c.Images.Dir); v != "" {
		return v
	}
	if !c.ImageURLEnabled() {
		return "" // 既不要 url 也没接对象存储：不落盘
	}
	return stateSibling(c.StateFile, "images")
}

// stateSibling 返回与 state 文件同目录的指定路径（相对路径场景回落当前目录）。
func stateSibling(stateFile, name string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}

// resolveCallbackBase 决定浏览器授权的回调基址：配置优先，否则按 listen 推导。
func resolveCallbackBase(cfg *Config) string {
	if v := strings.TrimRight(strings.TrimSpace(cfg.Login.CallbackBase), "/"); v != "" {
		return v
	}
	return callbackBase(cfg.Listen)
}

// callbackBase 按监听地址推导回调基址（配置未填时用）。
//
// 监听在 0.0.0.0 / 空 host 时按 127.0.0.1 拼（本机场景）；
// 监听在具体非环回 IP 时返回空，面板会提示改用「粘贴凭证」或配 login.callback_base。
func callbackBase(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return ""
	}
	switch host {
	case "", "0.0.0.0", "[::]", "::":
		host = "127.0.0.1"
	case "127.0.0.1", "localhost":
		host = "127.0.0.1"
	default:
		return ""
	}
	if port == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%s", host, port)
}

// portOf 从 listen 提取 ":port" 供启动日志拼 URL。
func portOf(listen string) string {
	i := strings.LastIndex(listen, ":")
	if i < 0 {
		return listen
	}
	return listen[i:]
}
