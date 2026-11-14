package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"tgdrama/internal/config"
	"tgdrama/internal/parser"
	"tgdrama/internal/storage"
	"tgdrama/internal/tgclient"
	"tgdrama/internal/web"
)

func main() {
	boot := time.Now()
	defer func() { log.Printf("[boot] 进程内总计 %s", time.Since(boot).Round(time.Millisecond)) }()
	configPath := flag.String("config", "", "配置文件路径（默认依次查找数据目录、可执行文件旁、当前目录下的 config.yaml）")
	port := flag.Int("port", 5278, "监听端口（被占用时自动向后顺延）")
	bind := flag.String("bind", "", "监听地址（插件宿主一般传 127.0.0.1）")
	lan := flag.Bool("lan", false, "对外提供访问：监听 0.0.0.0 并打印局域网入口")
	dataDir := flag.String("data", "", "数据目录（插件宿主传 getFilesDir()；桌面端默认在可执行文件旁）")
	compactOnly := flag.Bool("compact", false, "只压缩本地数据库（裁剪超长文案并 VACUUM），不连 Telegram、不启动服务")
	flag.Parse()

	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })

	customData := strings.TrimSpace(*dataDir)
	if customData != "" {
		customData = resolveDataDir(customData)
		if err := os.MkdirAll(customData, 0o755); err != nil {
			log.Fatalf("创建数据目录失败: %v", err)
		}
	}

	cfg, err := loadConfig(*configPath, customData, set["config"])
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if customData != "" {
		cfg.Data.Dir = customData
	}
	if err := cfg.EnsureDirs(); err != nil {
		log.Fatalf("初始化数据目录失败: %v", err)
	}
	log.Printf("Telegram 连接方式: MTProto（transport=%s，api_id=%d，走代理=%v）", cfg.Telegram.Transport, cfg.Telegram.APIID, cfg.Telegram.Proxy != "")

	store, err := storage.Open(cfg.DBPath())
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer store.Close()
	log.Printf("[boot] 打开数据库 %s", time.Since(boot).Round(time.Millisecond))

	if *compactOnly {
		n, err := store.ForceCompactIndex()
		if err != nil {
			log.Fatalf("压缩索引失败: %v", err)
		}
		hidden, err := store.RecomputeHiddenShows()
		if err != nil {
			log.Printf("标记非短剧条目失败: %v", err)
		}
		log.Printf("压缩完成：裁剪 %d 条超长文案，隐藏 %d 个非短剧条目", n, hidden)
		return
	}

	started := time.Now()

	if err := store.RebuildFromRaw(parser.New()); err != nil {
		log.Printf("重建媒体库映射失败: %v", err)
	}
	rebuilt := time.Since(started)

	if err := store.MergeDuplicateShows(); err != nil {
		log.Printf("合并重复短剧失败: %v", err)
	}

	if err := store.DeleteEmptyEpisodes(); err != nil {
		log.Printf("清理空集失败: %v", err)
	}
	if err := store.CleanJunkTags(); err != nil {
		log.Printf("清理标签失败: %v", err)
	}

	if err := store.CleanupEmptyShows(); err != nil {
		log.Printf("清理空壳短剧失败: %v", err)
	}

	if n, err := store.CompactIndex(); err != nil {
		log.Printf("压缩索引失败: %v", err)
	} else if n > 0 {
		log.Printf("已压缩 %d 条原始消息的文档信息（去掉内联缩略图与冗余字段）", n)
	}

	if n, err := store.RecomputeHiddenShows(); err != nil {
		log.Printf("标记非短剧条目失败: %v", err)
	} else if n > 0 {
		log.Printf("已隐藏 %d 个非短剧条目（泛分类词/拉群引流）", n)
	}
	elapsed := time.Since(started)
	log.Printf("启动维护完成：重建 %s，其余清理 %s（累计 %s）", rebuilt.Round(time.Millisecond), (elapsed - rebuilt).Round(time.Millisecond), time.Since(boot).Round(time.Millisecond))

	svc, err := tgclient.New(cfg, store)
	if err != nil {
		log.Fatalf("初始化 Telegram 客户端失败: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {

		if err := svc.Run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("Telegram 客户端异常（Web 服务继续运行）: %v", err)
		}
	}()

	addr := resolveAddr(*bind, *port, cfg.Server.Listen, *lan, set)
	ln, realPort, err := listenAvailable(addr, 40)
	if err != nil {
		log.Fatalf("端口不可用: %v", err)
	}
	writePortFile(cfg.Data.Dir, realPort)

	fmt.Printf("[jk] port=%d\n", realPort)

	webSrv := web.NewServer(store, svc)

	webSrv.SetCacheDir(cfg.CacheDir())

	webSrv.SetStreamOptions(cfg.Stream.ChunkKB, cfg.Stream.CacheMB, cfg.Stream.DiskCacheMB)

	srv := &http.Server{
		Handler:           webSrv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() {
		log.Printf("媒体库 Web 服务已启动: http://%s", ln.Addr().String())
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Web 服务异常: %v", err)
		}
	}()

	if *lan {
		for _, ip := range lanAddresses() {
			log.Printf("局域网访问: http://%s:%d", ip, realPort)
		}
	}

	<-ctx.Done()
	log.Println("正在关闭服务...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	log.Println("已退出")
}

func loadConfig(path, dataDir string, explicit bool) (*config.Config, error) {
	if explicit {
		return config.Load(path)
	}
	var cands []string
	if dataDir != "" {
		cands = append(cands, filepath.Join(dataDir, "config.yaml"))
	}
	if exe, err := os.Executable(); err == nil && exe != "" {
		cands = append(cands, filepath.Join(filepath.Dir(exe), "config.yaml"))
	}
	cands = append(cands, "config.yaml")
	for _, p := range cands {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			cfg, err := config.Load(p)
			if err != nil {
				return nil, err
			}
			log.Printf("使用配置文件: %s", p)
			return cfg, nil
		}
	}
	log.Printf("未找到 config.yaml，使用内置默认配置（Web 端链路，无需 api_id）")
	return config.Default(), nil
}

func resolveAddr(bind string, port int, listen string, lan bool, set map[string]bool) string {
	if lan {
		return net.JoinHostPort("0.0.0.0", strconv.Itoa(port))
	}
	if set["port"] || set["bind"] {
		return net.JoinHostPort(bind, strconv.Itoa(port))
	}
	if listen != "" {
		return listen
	}
	return net.JoinHostPort(bind, strconv.Itoa(port))
}

func listenAvailable(addr string, tries int) (net.Listener, int, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, 0, err
	}
	start, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, 0, err
	}
	var lastErr error
	for i := 0; i < tries; i++ {
		p := start + i
		if p > 65535 {
			break
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(p)))
		if err == nil {
			return ln, p, nil
		}
		lastErr = err
	}
	return nil, 0, lastErr
}

func resolveDataDir(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

func writePortFile(dir string, port int) {
	path := filepath.Join(dir, "port.txt")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(port)), 0o644); err != nil {
		log.Printf("写入端口文件失败: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("写入端口文件失败: %v", err)
	}
}

func lanAddresses() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil || ip.IsLoopback() {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			out = append(out, v4.String())
		}
	}
	return out
}
