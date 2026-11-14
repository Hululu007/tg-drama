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

const (
	coverMaxWidth = 480
	photoMaxWidth = 720
	webpQuality   = 78
	webpMethod    = 2
	webpMaxPixels = 2400 * 2400
)

var transcodeSem = make(chan struct{}, 4)

var transcodeFailLog sync.Map

var transcodeLocks sync.Map

func lockTranscode(key string) func() {
	v, _ := transcodeLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (s *Server) serveImage(w http.ResponseWriter, r *http.Request, src, key string, maxW int) bool {
	return s.serveImageCached(w, r, src, key, maxW, "")
}

func (s *Server) serveImageCached(w http.ResponseWriter, r *http.Request, src, key string, maxW int, cacheControl string) bool {
	fi, err := os.Stat(src)
	if err != nil || fi.IsDir() || fi.Size() == 0 {
		return false
	}

	srcTag := strconv.FormatInt(fi.ModTime().Unix(), 36) + "-" + strconv.FormatInt(fi.Size(), 36)

	if s.cacheDir == "" || !strings.Contains(r.Header.Get("Accept"), "image/webp") {
		serveImageFile(w, r, src, srcTag, "", cacheControl)
		return true
	}

	dst := filepath.Join(s.cacheDir, "webp", key+".webp")
	if di, err := os.Stat(dst); err == nil && di.Size() > 0 && !di.ModTime().Before(fi.ModTime()) {
		serveImageFile(w, r, dst, srcTag, "image/webp", cacheControl)
		return true
	}

	unlock := lockTranscode(dst)
	defer unlock()
	if di, err := os.Stat(dst); err == nil && di.Size() > 0 && !di.ModTime().Before(fi.ModTime()) {
		serveImageFile(w, r, dst, srcTag, "image/webp", cacheControl)
		return true
	}
	if err := s.transcodeWebP(src, dst, maxW); err != nil {

		if _, seen := transcodeFailLog.LoadOrStore(src, true); !seen {
			log.Printf("图片转 WebP 失败 %s: %v（回退原图）", filepath.Base(src), err)
		}
		serveImageFile(w, r, src, srcTag, "", cacheControl)
		return true
	}
	serveImageFile(w, r, dst, srcTag, "image/webp", cacheControl)
	return true
}

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

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir() && fi.Size() > 0
}

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
