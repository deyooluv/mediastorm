package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"novastream/internal/requestsecurity"

	"golang.org/x/image/draw"
	// TMDB can serve WebP bytes from .jpg URLs; register the decoder so the
	// proxy can transcode them instead of failing with a 500.
	_ "golang.org/x/image/webp"
)

const (
	imageProxyDefaultQuality = 80
	imageProxyMaxWidth       = 3840
	imageProxyMaxBytes       = 20 << 20
	imageProxyMaxDimension   = 8192
	imageProxyMaxPixels      = 40_000_000
)

type imageWarmRequest struct {
	Images []imageWarmItem `json:"images"`
}

type imageWarmItem struct {
	URL     string `json:"url"`
	Width   int    `json:"width,omitempty"`
	Quality int    `json:"quality,omitempty"`
}

type imageWarmResult struct {
	URL     string `json:"url"`
	Width   int    `json:"width,omitempty"`
	Quality int    `json:"quality,omitempty"`
	Cached  bool   `json:"cached"`
	Error   string `json:"error,omitempty"`
}

type imageWarmResponse struct {
	Results []imageWarmResult `json:"results"`
	Warmed  int               `json:"warmed"`
	Cached  int               `json:"cached"`
	Failed  int               `json:"failed"`
}

// ImageHandler handles image proxying with resize and caching
type ImageHandler struct {
	cacheDir   string
	httpc      *http.Client
	mu         sync.RWMutex
	inProgress map[string]chan struct{} // Prevent duplicate fetches
}

// NewImageHandler creates a new image proxy handler
func NewImageHandler(cacheDir string) *ImageHandler {
	// Create cache directory if needed
	imgCacheDir := filepath.Join(cacheDir, "images")
	if err := os.MkdirAll(imgCacheDir, 0755); err != nil {
		log.Printf("[ImageProxy] Warning: could not create cache dir %s: %v", imgCacheDir, err)
	}

	return &ImageHandler{
		cacheDir:   imgCacheDir,
		httpc:      requestsecurity.NewSafeHTTPClient(30*time.Second, 5, nil),
		inProgress: make(map[string]chan struct{}),
	}
}

// Proxy handles image proxy requests
// Query params:
//   - url: source image URL (required)
//   - w: target width (optional, default: original)
//   - q: JPEG quality 1-100 (optional, default: 80)
//   - reject_blank: return an error for fully black or transparent images (optional)
func (h *ImageHandler) Proxy(w http.ResponseWriter, r *http.Request) {
	sourceURL := r.URL.Query().Get("url")

	if sourceURL == "" {
		http.Error(w, "url parameter required", http.StatusBadRequest)
		return
	}

	if err := validateProxyImageURL(sourceURL); err != nil {
		http.Error(w, "invalid URL", http.StatusBadRequest)
		return
	}

	// Parse target width (0 = original size)
	targetWidth := 0
	if wStr := r.URL.Query().Get("w"); wStr != "" {
		if w, err := strconv.Atoi(wStr); err == nil {
			targetWidth = normalizeProxyWidth(w)
		}
	}

	// JPEG quality (default 80, good balance of size and quality)
	quality := imageProxyDefaultQuality
	if qStr := r.URL.Query().Get("q"); qStr != "" {
		if q, err := strconv.Atoi(qStr); err == nil && q >= 1 && q <= 100 {
			quality = q
		}
	}

	_, data, cached, err := h.ensureCached(sourceURL, targetWidth, quality)
	if err != nil {
		status := http.StatusBadGateway
		if strings.Contains(err.Error(), "decode") || strings.Contains(err.Error(), "encode") {
			status = http.StatusInternalServerError
		}
		http.Error(w, err.Error(), status)
		return
	}
	if r.URL.Query().Get("reject_blank") == "1" && isBlankProxyImageData(data) {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "image is blank", http.StatusUnprocessableEntity)
		return
	}

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=2592000") // 30 days
	if cached {
		w.Header().Set("X-Cache", "HIT")
	} else {
		w.Header().Set("X-Cache", "MISS")
	}
	w.Write(data)
}

// isBlankProxyImageData reports whether every decoded pixel is effectively
// transparent or black. A small RGB tolerance accounts for JPEG compression
// around an otherwise solid-black source image.
func isBlankProxyImageData(data []byte) bool {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return false
	}

	const (
		alphaThreshold = uint32(8 * 0x101)
		blackThreshold = uint32(8 * 0x101)
	)
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, a := img.At(x, y).RGBA()
			if a > alphaThreshold && (r > blackThreshold || g > blackThreshold || b > blackThreshold) {
				return false
			}
		}
	}
	return true
}

func validateProxyImageURL(sourceURL string) error {
	allowedImageHosts := map[string]struct{}{
		"image.tmdb.org":       {},
		"img.youtube.com":      {},
		"artworks.thetvdb.com": {},
		"thetvdb.com":          {},
		"www.thetvdb.com":      {},
	}
	parsedSource, err := url.Parse(sourceURL)
	if err != nil || parsedSource.Host == "" {
		return fmt.Errorf("invalid URL")
	}
	if _, ok := allowedImageHosts[parsedSource.Hostname()]; !ok {
		return fmt.Errorf("URL not allowed")
	}
	return nil
}

func validateExternalGIFURL(sourceURL string) error {
	parsedSource, err := url.Parse(sourceURL)
	if err != nil || parsedSource.Host == "" {
		return fmt.Errorf("invalid URL")
	}
	if parsedSource.Scheme != "https" && parsedSource.Scheme != "http" {
		return fmt.Errorf("invalid URL scheme")
	}
	if !strings.HasSuffix(strings.ToLower(parsedSource.Path), ".gif") {
		return fmt.Errorf("URL must point to a GIF")
	}
	host := parsedSource.Hostname()
	if host == "" || strings.EqualFold(host, "localhost") {
		return fmt.Errorf("URL host not allowed")
	}
	return requestsecurity.ValidateOutboundURL(context.Background(), sourceURL, nil)
}

func readBoundedImage(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, imageProxyMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > imageProxyMaxBytes {
		return nil, fmt.Errorf("image exceeds size limit")
	}
	return data, nil
}

func validateImageDimensions(config image.Config) error {
	if config.Width <= 0 || config.Height <= 0 || config.Width > imageProxyMaxDimension || config.Height > imageProxyMaxDimension {
		return fmt.Errorf("image dimensions exceed limit")
	}
	if int64(config.Width)*int64(config.Height) > imageProxyMaxPixels {
		return fmt.Errorf("image pixel count exceeds limit")
	}
	return nil
}

// prepareProxyImage preserves JPEG source bytes when no downscale is required.
// Re-encoding an already correctly-sized JPEG only adds generation loss, which
// is especially visible in large TV hero artwork. Images that do need resizing
// are decoded, downscaled, and encoded at the requested quality.
func prepareProxyImage(imageData []byte, targetWidth, quality int) ([]byte, error) {
	imageConfig, imageFormat, err := image.DecodeConfig(bytes.NewReader(imageData))
	if err != nil {
		return nil, fmt.Errorf("failed to decode image")
	}
	if err := validateImageDimensions(imageConfig); err != nil {
		return nil, err
	}

	if imageFormat == "jpeg" && (targetWidth <= 0 || targetWidth >= imageConfig.Width) {
		return imageData, nil
	}

	img, _, err := image.Decode(bytes.NewReader(imageData))
	if err != nil {
		return nil, fmt.Errorf("failed to decode image")
	}

	if targetWidth > 0 && targetWidth < imageConfig.Width {
		targetHeight := int(float64(imageConfig.Height) * (float64(targetWidth) / float64(imageConfig.Width)))
		dst := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Over, nil)
		img = dst
	}

	var output bytes.Buffer
	if err := jpeg.Encode(&output, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("failed to encode image")
	}
	return output.Bytes(), nil
}

func normalizeProxyWidth(width int) int {
	if width <= 0 {
		return 0
	}
	return min(width, imageProxyMaxWidth)
}

func normalizeProxyQuality(quality int) int {
	if quality >= 1 && quality <= 100 {
		return quality
	}
	return imageProxyDefaultQuality
}

func (h *ImageHandler) ensureCached(sourceURL string, targetWidth, quality int) (string, []byte, bool, error) {
	cacheKey := h.cacheKey(sourceURL, targetWidth, quality)
	cachePath := filepath.Join(h.cacheDir, cacheKey+".jpg")
	if data, err := os.ReadFile(cachePath); err == nil {
		return cachePath, data, true, nil
	}

	// Prevent duplicate fetches for the same image
	h.mu.Lock()
	if ch, exists := h.inProgress[cacheKey]; exists {
		h.mu.Unlock()
		// Wait for other request to finish
		<-ch
		// Now try to serve from cache
		if data, err := os.ReadFile(cachePath); err == nil {
			return cachePath, data, true, nil
		}
		return cachePath, nil, false, fmt.Errorf("failed to load image")
	}
	// Mark as in progress
	ch := make(chan struct{})
	h.inProgress[cacheKey] = ch
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.inProgress, cacheKey)
		close(ch)
		h.mu.Unlock()
	}()

	// Fetch the image
	client := *h.httpc
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return http.ErrUseLastResponse
		}
		if err := validateProxyImageURL(req.URL.String()); err != nil {
			return err
		}
		return requestsecurity.ValidateOutboundURL(req.Context(), req.URL.String(), nil)
	}
	resp, err := client.Get(sourceURL)
	if err != nil {
		log.Printf("[ImageProxy] Fetch error for %s: %v", requestsecurity.URLForLog(sourceURL), err)
		return cachePath, nil, false, fmt.Errorf("failed to fetch image")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("[ImageProxy] Fetch returned %d for %s", resp.StatusCode, requestsecurity.URLForLog(sourceURL))
		return cachePath, nil, false, fmt.Errorf("image source error")
	}

	// Decode the image
	imageData, err := readBoundedImage(resp.Body)
	if err != nil {
		return cachePath, nil, false, err
	}
	preparedData, err := prepareProxyImage(imageData, targetWidth, quality)
	if err != nil {
		log.Printf("[ImageProxy] Transform error for %s: %v", requestsecurity.URLForLog(sourceURL), err)
		return cachePath, nil, false, err
	}

	// Cache either the untouched JPEG source or the resized JPEG variant.
	tmpPath := cachePath + ".tmp"
	if err := os.WriteFile(tmpPath, preparedData, 0644); err != nil {
		log.Printf("[ImageProxy] Cache create error: %v", err)
		return cachePath, preparedData, false, nil
	}

	// Atomic rename
	if err := os.Rename(tmpPath, cachePath); err != nil {
		os.Remove(tmpPath)
		log.Printf("[ImageProxy] Cache rename error: %v", err)
	}

	// Serve from cache
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return cachePath, nil, false, fmt.Errorf("failed to read cached image")
	}

	return cachePath, data, false, nil
}

// GIFFirstFrame returns the first frame of an external GIF as a cached PNG.
func (h *ImageHandler) GIFFirstFrame(w http.ResponseWriter, r *http.Request) {
	sourceURL := strings.TrimSpace(r.URL.Query().Get("url"))
	if sourceURL == "" {
		http.Error(w, "url parameter required", http.StatusBadRequest)
		return
	}
	if err := validateExternalGIFURL(sourceURL); err != nil {
		http.Error(w, "invalid URL", http.StatusBadRequest)
		return
	}

	_, data, cached, err := h.ensureGIFFirstFrameCached(sourceURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=2592000")
	if cached {
		w.Header().Set("X-Cache", "HIT")
	} else {
		w.Header().Set("X-Cache", "MISS")
	}
	w.Write(data)
}

func (h *ImageHandler) ensureGIFFirstFrameCached(sourceURL string) (string, []byte, bool, error) {
	cacheKey := h.cacheKey("gif-first-frame:"+sourceURL, 0, 0)
	cachePath := filepath.Join(h.cacheDir, cacheKey+".png")
	if data, err := os.ReadFile(cachePath); err == nil {
		return cachePath, data, true, nil
	}

	h.mu.Lock()
	if ch, exists := h.inProgress[cacheKey]; exists {
		h.mu.Unlock()
		<-ch
		if data, err := os.ReadFile(cachePath); err == nil {
			return cachePath, data, true, nil
		}
		return cachePath, nil, false, fmt.Errorf("failed to load first frame")
	}
	ch := make(chan struct{})
	h.inProgress[cacheKey] = ch
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.inProgress, cacheKey)
		close(ch)
		h.mu.Unlock()
	}()

	client := *h.httpc
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return http.ErrUseLastResponse
		}
		if err := validateExternalGIFURL(req.URL.String()); err != nil {
			return err
		}
		return requestsecurity.ValidateOutboundURL(req.Context(), req.URL.String(), nil)
	}
	resp, err := client.Get(sourceURL)
	if err != nil {
		log.Printf("[ImageProxy] GIF first-frame fetch error for %s: %v", requestsecurity.URLForLog(sourceURL), err)
		return cachePath, nil, false, fmt.Errorf("failed to fetch image")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[ImageProxy] GIF first-frame fetch returned %d for %s", resp.StatusCode, requestsecurity.URLForLog(sourceURL))
		return cachePath, nil, false, fmt.Errorf("image source error")
	}

	gifData, err := readBoundedImage(resp.Body)
	if err != nil {
		return cachePath, nil, false, err
	}
	gifConfig, err := gif.DecodeConfig(bytes.NewReader(gifData))
	if err != nil {
		return cachePath, nil, false, fmt.Errorf("failed to decode GIF")
	}
	if err := validateImageDimensions(gifConfig); err != nil {
		return cachePath, nil, false, err
	}
	firstFrame, err := gif.Decode(bytes.NewReader(gifData))
	if err != nil {
		log.Printf("[ImageProxy] GIF first-frame decode error for %s: %v", requestsecurity.URLForLog(sourceURL), err)
		return cachePath, nil, false, fmt.Errorf("failed to decode GIF")
	}

	tmpPath := cachePath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		log.Printf("[ImageProxy] GIF first-frame cache create error: %v", err)
		var buf bytes.Buffer
		if encodeErr := png.Encode(&buf, firstFrame); encodeErr != nil {
			return cachePath, nil, false, fmt.Errorf("failed to encode first frame")
		}
		return cachePath, buf.Bytes(), false, nil
	}
	if err := png.Encode(f, firstFrame); err != nil {
		f.Close()
		os.Remove(tmpPath)
		log.Printf("[ImageProxy] GIF first-frame encode error: %v", err)
		return cachePath, nil, false, fmt.Errorf("failed to encode first frame")
	}
	f.Close()
	if err := os.Rename(tmpPath, cachePath); err != nil {
		os.Remove(tmpPath)
		log.Printf("[ImageProxy] GIF first-frame cache rename error: %v", err)
	}
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return cachePath, nil, false, fmt.Errorf("failed to read cached first frame")
	}
	return cachePath, data, false, nil
}

// Warm pre-fills the image proxy cache for a batch of source URLs and resize widths.
func (h *ImageHandler) Warm(w http.ResponseWriter, r *http.Request) {
	var req imageWarmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid request body"})
		return
	}
	if len(req.Images) > 64 {
		req.Images = req.Images[:64]
	}

	response := imageWarmResponse{Results: make([]imageWarmResult, 0, len(req.Images))}
	seen := make(map[string]struct{})
	for _, item := range req.Images {
		sourceURL := strings.TrimSpace(item.URL)
		width := normalizeProxyWidth(item.Width)
		quality := normalizeProxyQuality(item.Quality)
		result := imageWarmResult{URL: sourceURL, Width: width, Quality: quality}
		key := h.cacheKey(sourceURL, width, quality)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		if sourceURL == "" {
			result.Error = "url required"
			response.Failed++
			response.Results = append(response.Results, result)
			continue
		}
		if err := validateProxyImageURL(sourceURL); err != nil {
			result.Error = err.Error()
			response.Failed++
			response.Results = append(response.Results, result)
			continue
		}

		_, _, cached, err := h.ensureCached(sourceURL, width, quality)
		if err != nil {
			result.Error = err.Error()
			response.Failed++
		} else if cached {
			result.Cached = true
			response.Cached++
		} else {
			response.Warmed++
		}
		response.Results = append(response.Results, result)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// cacheKey generates a unique cache key for the image
func (h *ImageHandler) cacheKey(url string, width, quality int) string {
	data := fmt.Sprintf("%s|%d|%d", url, width, quality)
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:16]) // 32 char hex string
}

// Options handles CORS preflight
func (h *ImageHandler) Options(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// ClearCache removes all cached images
func (h *ImageHandler) ClearCache() error {
	entries, err := os.ReadDir(h.cacheDir)
	if err != nil {
		return err
	}

	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() && (strings.HasSuffix(entry.Name(), ".jpg") || strings.HasSuffix(entry.Name(), ".png")) {
			if err := os.Remove(filepath.Join(h.cacheDir, entry.Name())); err != nil {
				errs = append(errs, err)
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to remove %d files", len(errs))
	}
	return nil
}

// CacheStats returns cache statistics
func (h *ImageHandler) CacheStats() (count int, sizeBytes int64) {
	entries, err := os.ReadDir(h.cacheDir)
	if err != nil {
		return 0, 0
	}

	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jpg") {
			count++
			if info, err := entry.Info(); err == nil {
				sizeBytes += info.Size()
			}
		}
	}
	return
}

// Unused imports guard - these are actually used
var _ = jpeg.Encode
var _ = png.Decode
var _ = io.Copy
