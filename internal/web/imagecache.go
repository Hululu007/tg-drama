package web

import (
	"fmt"
	"image"
	"image/draw"
	"log"
	"math"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/gen2brain/webp"
	xdraw "golang.org/x/image/draw"

	_ "golang.org/x/image/webp"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

// 图片统一转成 WebP 再发：Telegram 的封面/预览图都是 JPEG，
// 直接发原图体积偏大（预览图平均 110KB），转码后能省 40%~60% 流量，
// 同时把过宽的图等比缩到屏幕需要的尺寸。
const (
	coverMaxWidth = 480         // 列表/详情的封面宽度
	photoMaxWidth = 720         // 详情页预览图的宽度
	webpQuality   = 78          // 画质与体积的折中，肉眼与原图无明显差别
	webpMethod    = 2           // 0 最快、6 最省体积；2 在两边之间
	webpMaxPixels = 2400 * 2400 // 单张图最大像素，超过先缩小，避免转码吃掉几百 MB 内存
)

// transcodeSem 限制同时进行的转码数：首屏 30 张封面会同时请求，
// 不限流的话 CPU 会被 30 个 libwebp 实例打满，反而拖慢页面。
var transcodeSem = make(chan struct{}, 4)

// transcodeFailLog 记录已经打过日志的文件，避免坏图每次请求都刷一条
var transcodeFailLog sync.Map

// transcodeLocks 按缓存文件加锁，避免同一张图被并发转码两遍。
// 不只是浪费 CPU：Windows 上 os.Rename 覆盖一个正在被 http.ServeFile 读的文件
// 会直接返回 Access is denied（实测日志里就有），于是这次请求退回原图、还多了
// 一条失败日志。加锁后同一 key 只有一个协程转码，其余等它完成后直接读结果。
var transcodeLocks sync.Map // key -> *sync.Mutex

func lockTranscode(key string) func() {
	v, _ := transcodeLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// serveImage 把 src 以 WebP 形式发出，成功返回 true（文件不存在或有异常返回 false，
// 由调用方决定回落到占位图）。key 用于定位缓存文件（如 "covers/12"），
// maxW 是输出的最大宽度（原图更窄时不放大）。
func (s *Server) serveImage(w http.ResponseWriter, r *http.Request, src, key string, maxW int) bool {
	return s.serveImageCached(w, r, src, key, maxW, "")
}

// serveImageCached 与 serveImage 相同，额外指定 Cache-Control。
// 预览图用封面占位时传 "no-cache"：真图落盘后浏览器必须回来重新校验，
// 否则会把封面一直当成这张预览图缓存下去。
func (s *Server) serveImageCached(w http.ResponseWriter, r *http.Request, src, key string, maxW int, cacheControl string) bool {
	fi, err := os.Stat(src)
	if err != nil || fi.IsDir() || fi.Size() == 0 {
		return false
	}
	// 原图的缓存标识：内容变化（封面上新）时 ETag 跟着变，缓存不必手工清理
	srcTag := strconv.FormatInt(fi.ModTime().Unix(), 36) + "-" + strconv.FormatInt(fi.Size(), 36)

	// 明确不支持 WebP 的客户端（很老的 WebView）或没配置缓存目录时直接发原图
	if s.cacheDir == "" || !strings.Contains(r.Header.Get("Accept"), "image/webp") {
		serveImageFile(w, r, src, srcTag, "", cacheControl)
		return true
	}

	dst := filepath.Join(s.cacheDir, "webp", key+".webp")
	if di, err := os.Stat(dst); err == nil && di.Size() > 0 && !di.ModTime().Before(fi.ModTime()) {
		serveImageFile(w, r, dst, srcTag, "image/webp", cacheControl)
		return true
	}
	// 同一张图只允许一个协程转码，其余等它写完后直接复用（顺带解决 Windows 上
	// rename 覆盖正在被读取的文件失败的问题）。
	unlock := lockTranscode(dst)
	defer unlock()
	if di, err := os.Stat(dst); err == nil && di.Size() > 0 && !di.ModTime().Before(fi.ModTime()) {
		serveImageFile(w, r, dst, srcTag, "image/webp", cacheControl)
		return true
	}
	if err := s.transcodeWebP(src, dst, maxW); err != nil {
		// 转码失败（格式不支持/坏图）就退回原图，页面不会出现空白
		if _, seen := transcodeFailLog.LoadOrStore(src, true); !seen {
			log.Printf("图片转 WebP 失败 %s: %v（回退原图）", filepath.Base(src), err)
		}
		serveImageFile(w, r, src, srcTag, "", cacheControl)
		return true
	}
	serveImageFile(w, r, dst, srcTag, "image/webp", cacheControl)
	return true
}

// serveImageFile 发本地图片文件，带 ETag 与一天缓存。
// ctype 为空时按扩展名推断（原图分支用）；cacheControl 为空时用默认的一周缓存。
func serveImageFile(w http.ResponseWriter, r *http.Request, path, etag, ctype, cacheControl string) {
	h := w.Header()
	if cacheControl == "" {
		cacheControl = "public, max-age=604800"
	}
	h.Set("Cache-Control", cacheControl)
	if ctype == "" {
		ctype = mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	}
	if ctype != "" {
		h.Set("Content-Type", ctype)
	}
	if etag != "" {
		h.Set("ETag", `"`+etag+`"`)
		if inm := r.Header.Get("If-None-Match"); inm != "" && strings.Contains(inm, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	http.ServeFile(w, r, path)
}

// transcodeWebP 把 src 解码、等比缩放后编码成 WebP 写到 dst（原子替换）。
// 解码与编码都跑在 recover 里：个别畸形图片会让编码器 panic，不能让它带崩整个服务。
func (s *Server) transcodeWebP(src, dst string, maxW int) (err error) {
	transcodeSem <- struct{}{}
	defer func() { <-transcodeSem }()

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("转码异常: %v", r)
		}
	}()

	f, err := os.Open(src)
	if err != nil {
		return err
	}
	img, _, err := image.Decode(f)
	_ = f.Close()
	if err != nil {
		return err
	}

	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return fmt.Errorf("图片尺寸异常")
	}
	w, h := b.Dx(), b.Dy()
	if maxW > 0 && w > maxW {
		h = h * maxW / w
		w = maxW
	}
	if int64(w)*int64(h) > webpMaxPixels {
		side := math.Sqrt(float64(webpMaxPixels) / float64(w*h))
		w = int(float64(w) * side)
		h = int(float64(h) * side)
		if w <= 0 || h <= 0 {
			return fmt.Errorf("图片尺寸异常")
		}
	}

	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	if w == b.Dx() && h == b.Dy() {
		draw.Draw(rgba, rgba.Bounds(), img, b.Min, draw.Src)
	} else {
		xdraw.ApproxBiLinear.Scale(rgba, rgba.Bounds(), img, b, draw.Src, nil)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	encErr := webp.Encode(tmp, rgba, webp.Options{Quality: webpQuality, Method: webpMethod})
	closeErr := tmp.Close()
	if encErr != nil || closeErr != nil {
		_ = os.Remove(tmpName)
		if encErr != nil {
			return encErr
		}
		return closeErr
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// fileExists 判断本地图片文件是否还在（清缓存后库里的路径会变成空串，或指向已删除的文件）
func fileExists(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir() && fi.Size() > 0
}

// ClearWebPCache 删掉转码缓存（清理 photos 时一并调用），返回释放的字节数
func (s *Server) ClearWebPCache(kind string) int64 {
	if s.cacheDir == "" {
		return 0
	}
	dir := filepath.Join(s.cacheDir, "webp", kind)
	var freed int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if info, err := e.Info(); err == nil {
			freed += info.Size()
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
	return freed
}
