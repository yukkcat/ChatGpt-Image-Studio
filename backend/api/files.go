package api

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const defaultImageDir = "data/tmp/image"

// downloadAndCache downloads an upstream image using the image client's transport
// (Chrome TLS fingerprint), saves to local disk, and returns the local filename.
func downloadAndCache(client imageDownloader, upstreamURL string, cacheDir string) (string, error) {
	// Generate a stable filename from the URL
	hash := sha256.Sum256([]byte(upstreamURL))
	filename := fmt.Sprintf("%x.png", hash[:12])
	dir := firstNonEmpty(cacheDir, defaultImageDir)
	localPath := filepath.Join(dir, filename)

	// Check cache
	if _, err := os.Stat(localPath); err == nil {
		return filename, nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	data, err := client.DownloadBytes(upstreamURL)
	if err != nil {
		return "", fmt.Errorf("download upstream image: %w", err)
	}

	tmpFile := localPath + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmpFile, localPath); err != nil {
		return "", err
	}

	slog.Info("cached image", "file", filename, "size", len(data))
	return filename, nil
}

func (s *Server) publicImageURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || s == nil || s.cfg == nil {
		return trimmed
	}
	if strings.HasPrefix(strings.ToLower(trimmed), "http://") || strings.HasPrefix(strings.ToLower(trimmed), "https://") {
		return trimmed
	}
	if !strings.HasPrefix(trimmed, "/v1/files/image/") {
		return trimmed
	}
	baseURL := strings.TrimRight(strings.TrimSpace(s.cfg.App.PublicImageBaseURL), "/")
	if baseURL == "" {
		return trimmed
	}
	return baseURL + trimmed
}

func (s *Server) cachedImageURL(r *http.Request, filename string) string {
	relative := "/v1/files/image/" + filename
	if strings.TrimSpace(s.cfg.App.PublicImageBaseURL) != "" {
		return s.publicImageURL(relative)
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s%s", scheme, r.Host, relative)
}

func (s *Server) resolveImageFilePath(name string) string {
	baseName := filepath.Base(strings.TrimSpace(name))
	candidates := []string{
		filepath.Join(s.cfg.ResolvePath(s.cfg.Storage.ImageDir), baseName),
	}
	legacyPath := filepath.Join(s.cfg.ResolvePath(defaultImageDir), baseName)
	if !strings.EqualFold(filepath.Clean(legacyPath), filepath.Clean(candidates[0])) {
		candidates = append(candidates, legacyPath)
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	return s.searchImageFilePathFallback(baseName)
}

func (s *Server) searchImageFilePathFallback(name string) string {
	baseName := filepath.Base(strings.TrimSpace(name))
	if baseName == "" || s == nil || s.cfg == nil {
		return ""
	}

	dataRoot := filepath.Join(s.cfg.Paths().Root, "data")
	info, err := os.Stat(dataRoot)
	if err != nil || !info.IsDir() {
		return ""
	}

	var found string
	_ = filepath.WalkDir(dataRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || found != "" {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !strings.EqualFold(entry.Name(), baseName) {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		found = path
		return fs.SkipAll
	})
	return found
}

// handleImageFile serves cached and server-stored images from storage.image_dir.
func (s *Server) handleImageFile(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/v1/files/image/")
	name = strings.ReplaceAll(name, "/", "-")
	if name == "" {
		writeError(w, http.StatusNotFound, "image not found")
		return
	}

	path := s.resolveImageFilePath(name)
	if path == "" {
		writeError(w, http.StatusNotFound, "image not found")
		return
	}

	ext := strings.ToLower(filepath.Ext(path))
	contentTypes := map[string]string{
		".png":  "image/png",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".webp": "image/webp",
		".gif":  "image/gif",
	}
	ct := contentTypes[ext]
	if ct == "" {
		ct = "image/png"
	}

	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Type", ct)
	http.ServeFile(w, r, path)
}
