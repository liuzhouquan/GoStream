package platform

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func (s *Server) upload(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 250<<20)
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
	if err = os.MkdirAll(s.MediaDir, 0755); err != nil {
		respondError(c, 500, "media storage unavailable")
		return
	}
	name := fmt.Sprintf("%d-%d%s", uid(c), time.Now().UnixNano(), ext)
	dst, err := os.Create(filepath.Join(s.MediaDir, name))
	if err != nil {
		respondError(c, 500, "media storage unavailable")
		return
	}
	n, copyErr := dst.ReadFrom(f)
	closeErr := dst.Close()
	if copyErr != nil || closeErr != nil || n != h.Size {
		_ = os.Remove(filepath.Join(s.MediaDir, name))
		respondError(c, 500, "upload failed")
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
	if in.Title == "" || len(in.Title) > 120 || in.MediaURL == "" {
		respondError(c, 400, "title and media_url are required")
		return
	}
	v := Video{AuthorID: uid(c), Title: in.Title, Description: in.Description, MediaURL: in.MediaURL, CoverURL: in.CoverURL, Status: "published"}
	if s.DB.Create(&v).Error != nil {
		respondError(c, 500, "could not publish video")
		return
	}
	s.DB.Preload("Author").First(&v, v.ID)
	c.JSON(201, v)
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
	if s.DB.First(&v, c.Param("id")).Error != nil {
		respondError(c, 404, "video not found")
		return
	}
	if v.AuthorID != uid(c) {
		respondError(c, 403, "only the author can delete this video")
		return
	}
	s.DB.Model(&v).Update("status", "deleted")
	c.Status(204)
}

func (s *Server) userVideos(c *gin.Context) {
	var items []Video
	s.DB.Preload("Author").Where("author_id = ? AND status = ?", c.Param("id"), "published").Order("created_at DESC").Limit(50).Find(&items)
	c.JSON(200, gin.H{"items": items})
}
