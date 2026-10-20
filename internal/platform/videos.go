package platform

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
)

func mediaKind(ext string, header []byte) string {
	switch ext {
	case ".mp4", ".mov":
		if len(header) >= 12 && string(header[4:8]) == "ftyp" {
			return "video"
		}
	case ".webm":
		if len(header) >= 8 && bytes.Equal(header[:4], []byte{0x1a, 0x45, 0xdf, 0xa3}) && bytes.Contains(header, []byte("webm")) {
			return "video"
		}
	case ".jpg", ".jpeg":
		if http.DetectContentType(header) == "image/jpeg" {
			return "image"
		}
	case ".png":
		if http.DetectContentType(header) == "image/png" {
			return "image"
		}
	case ".webp":
		if http.DetectContentType(header) == "image/webp" {
			return "image"
		}
	}
	return ""
}

func validImageFile(path, ext string) bool {
	if ext == ".webp" {
		return true
	} // WebP has a checked signature; no stdlib decoder is available.
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	config, format, err := image.DecodeConfig(f)
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 8192 || config.Height > 8192 {
		return false
	}
	if ext == ".png" {
		return format == "png"
	}
	return format == "jpeg"
}

func (s *Server) upload(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 250<<20)
	defer func() {
		if c.Request.MultipartForm != nil {
			_ = c.Request.MultipartForm.RemoveAll()
		}
	}()
	f, h, err := c.Request.FormFile("file")
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			respondError(c, 413, "file exceeds 250 MiB limit")
			return
		}
		respondError(c, 400, "file is required")
		return
	}
	defer f.Close()
	if h.Size <= 0 || h.Size > 250<<20 {
		respondError(c, 413, "file must be between 1 byte and 250 MB")
		return
	}
	ext := strings.ToLower(filepath.Ext(h.Filename))
	allowed := map[string]bool{".mp4": true, ".webm": true, ".mov": true, ".jpg": true, ".jpeg": true, ".png": true, ".webp": true}
	if !allowed[ext] {
		respondError(c, 400, "unsupported media extension")
		return
	}
	header := make([]byte, 512)
	nHeader, readErr := io.ReadFull(f, header)
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		respondError(c, 400, "could not inspect media")
		return
	}
	kind := mediaKind(ext, header[:nHeader])
	if kind == "" {
		respondError(c, 400, "file content does not match a supported media type")
		return
	}
	if err = os.MkdirAll(s.MediaDir, 0755); err != nil {
		respondError(c, 500, "media storage unavailable")
		return
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		respondError(c, 500, "could not allocate media name")
		return
	}
	name := hex.EncodeToString(nonce[:]) + ext
	dst, err := os.OpenFile(filepath.Join(s.MediaDir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		respondError(c, 500, "media storage unavailable")
		return
	}
	n, copyErr := io.Copy(dst, io.MultiReader(bytes.NewReader(header[:nHeader]), f))
	closeErr := dst.Close()
	if copyErr != nil || closeErr != nil || n != h.Size {
		_ = os.Remove(filepath.Join(s.MediaDir, name))
		respondError(c, 500, "upload failed")
		return
	}
	if kind == "image" && !validImageFile(filepath.Join(s.MediaDir, name), ext) {
		_ = os.Remove(filepath.Join(s.MediaDir, name))
		respondError(c, 400, "invalid image file")
		return
	}
	asset := MediaAsset{OwnerID: uid(c), URL: "/media/" + name, Kind: kind, Size: n}
	if err := s.DB.Create(&asset).Error; err != nil {
		_ = os.Remove(filepath.Join(s.MediaDir, name))
		respondError(c, 500, "could not register media")
		return
	}
	c.JSON(201, gin.H{"url": "/media/" + name, "size": n, "filename": name})
}

func (s *Server) createVideo(c *gin.Context) {
	var in struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		MediaURL    string `json:"media_url"`
		CoverURL    string `json:"cover_url"`
	}
	if c.ShouldBindJSON(&in) != nil {
		respondError(c, 400, "invalid JSON")
		return
	}
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" || len(in.Title) > 120 || len(in.Description) > 2000 || in.MediaURL == "" {
		respondError(c, 400, "title and media_url are required")
		return
	}
	if !s.ownsMedia(uid(c), in.MediaURL, "video") {
		respondError(c, 400, "upload a video with this account before publishing")
		return
	}
	if in.CoverURL != "" {
		if !s.ownsMedia(uid(c), in.CoverURL, "image") {
			respondError(c, 400, "cover must be an image uploaded by this account")
			return
		}
	}
	v := Video{AuthorID: uid(c), Title: in.Title, Description: in.Description, MediaURL: in.MediaURL, CoverURL: in.CoverURL, Status: "published"}
	if s.DB.Create(&v).Error != nil {
		respondError(c, 500, "could not publish video")
		return
	}
	s.invalidateFeed(c.Request.Context())
	s.DB.Preload("Author").First(&v, v.ID)
	c.JSON(201, v)
}

func (s *Server) ownsMedia(ownerID uint, url, kind string) bool {
	var count int64
	return s.DB.Model(&MediaAsset{}).Where("url = ? AND owner_id = ? AND kind = ?", url, ownerID, kind).Count(&count).Error == nil && count == 1
}

func (s *Server) getVideo(c *gin.Context) {
	var v Video
	if s.DB.Preload("Author").First(&v, c.Param("id")).Error != nil || v.Status != "published" {
		respondError(c, 404, "video not found")
		return
	}
	c.JSON(200, v)
}

func (s *Server) deleteVideo(c *gin.Context) {
	var v Video
	if s.DB.Where("id = ? AND status = ?", c.Param("id"), "published").First(&v).Error != nil {
		respondError(c, 404, "video not found")
		return
	}
	if v.AuthorID != uid(c) {
		respondError(c, 403, "only the author can delete this video")
		return
	}
	if err := s.DB.Model(&v).Update("status", "deleted").Error; err != nil {
		respondError(c, 500, "could not delete video")
		return
	}
	s.invalidateFeed(c.Request.Context())
	c.Status(204)
}

func (s *Server) updateVideo(c *gin.Context) {
	var v Video
	if s.DB.Where("id = ? AND status = ?", c.Param("id"), "published").First(&v).Error != nil {
		respondError(c, 404, "video not found")
		return
	}
	if v.AuthorID != uid(c) {
		respondError(c, 403, "only the author can edit this video")
		return
	}
	var in struct {
		Title       *string `json:"title"`
		Description *string `json:"description"`
		MediaURL    *string `json:"media_url"`
		CoverURL    *string `json:"cover_url"`
	}
	if c.ShouldBindJSON(&in) != nil {
		respondError(c, 400, "invalid JSON")
		return
	}
	updates := map[string]any{}
	if in.Title != nil {
		title := strings.TrimSpace(*in.Title)
		if title == "" || len(title) > 120 {
			respondError(c, 400, "title must be 1-120 characters")
			return
		}
		updates["title"] = title
	}
	if in.Description != nil {
		if len(*in.Description) > 2000 {
			respondError(c, 400, "description is too long")
			return
		}
		updates["description"] = strings.TrimSpace(*in.Description)
	}
	if in.MediaURL != nil {
		if !s.ownsMedia(uid(c), *in.MediaURL, "video") {
			respondError(c, 400, "upload a video with this account before using it")
			return
		}
		updates["media_url"] = *in.MediaURL
	}
	if in.CoverURL != nil {
		if *in.CoverURL != "" && !s.ownsMedia(uid(c), *in.CoverURL, "image") {
			respondError(c, 400, "cover must be an image uploaded by this account")
			return
		}
		updates["cover_url"] = *in.CoverURL
	}
	if len(updates) == 0 {
		respondError(c, 400, "no video fields supplied")
		return
	}
	if err := s.DB.Model(&v).Updates(updates).Error; err != nil {
		respondError(c, 500, "could not edit video")
		return
	}
	s.invalidateFeed(c.Request.Context())
	if err := s.DB.Preload("Author").First(&v, v.ID).Error; err != nil {
		respondError(c, 500, "could not load video")
		return
	}
	c.JSON(200, v)
}

func (s *Server) userVideos(c *gin.Context) {
	var items []Video
	if err := s.DB.Preload("Author").Where("author_id = ? AND status = ?", c.Param("id"), "published").Order("created_at DESC").Limit(50).Find(&items).Error; err != nil {
		respondError(c, 500, "could not load user videos")
		return
	}
	c.JSON(200, gin.H{"items": items})
}
