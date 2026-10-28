package platform

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/image/webp"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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

const maxImagePixels int64 = 16_000_000

func validImageFile(path, ext string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var config image.Config
	var format string
	if ext == ".webp" {
		info, err := f.Stat()
		if err != nil || info.Size() < 12 {
			return false
		}
		var header [12]byte
		if _, err := io.ReadFull(f, header[:]); err != nil || string(header[:4]) != "RIFF" || string(header[8:]) != "WEBP" ||
			uint64(binary.LittleEndian.Uint32(header[4:8]))+8 != uint64(info.Size()) {
			return false
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return false
		}
		config, err = webp.DecodeConfig(f)
		format = "webp"
	} else {
		config, format, err = image.DecodeConfig(f)
	}
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 8192 || config.Height > 8192 ||
		int64(config.Width)*int64(config.Height) > maxImagePixels {
		return false
	}
	switch ext {
	case ".png":
		if format != "png" {
			return false
		}
	case ".jpg", ".jpeg":
		if format != "jpeg" {
			return false
		}
	case ".webp":
	default:
		return false
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false
	}
	var decoded image.Image
	if ext == ".webp" {
		decoded, err = webp.Decode(f)
	} else {
		var decodedFormat string
		decoded, decodedFormat, err = image.Decode(f)
		if decodedFormat != format {
			return false
		}
	}
	return err == nil && decoded.Bounds().Dx() == config.Width && decoded.Bounds().Dy() == config.Height
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
	if s.objectMedia != nil {
		s.uploadObjectFile(c, io.MultiReader(bytes.NewReader(header[:nHeader]), f), h.Size, name, ext, kind)
		return
	}
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

func (s *Server) uploadObjectFile(c *gin.Context, source io.Reader, size int64, name, ext, kind string) {
	tmp, err := os.CreateTemp("", "gostream-upload-*")
	if err != nil {
		respondError(c, 500, "media storage unavailable")
		return
	}
	defer os.Remove(tmp.Name())
	n, copyErr := io.Copy(tmp, source)
	closeErr := tmp.Close()
	if copyErr != nil || closeErr != nil || n != size {
		respondError(c, 500, "upload failed")
		return
	}
	if kind == "image" && !validImageFile(tmp.Name(), ext) {
		respondError(c, 400, "invalid image file")
		return
	}
	key := "media/" + name
	if err := s.objectMedia.putFile(c.Request.Context(), key, tmp.Name()); err != nil {
		respondError(c, 503, "media storage unavailable")
		return
	}
	asset := MediaAsset{OwnerID: uid(c), URL: "/media/" + name, Kind: kind, Size: n}
	if err := s.DB.WithContext(c.Request.Context()).Create(&asset).Error; err != nil {
		s.objectMedia.removeBestEffort(key)
		respondError(c, 500, "could not register media")
		return
	}
	c.JSON(201, gin.H{"url": asset.URL, "size": n, "filename": name})
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
	status := "published"
	if s.ffmpegBin != "" {
		status = "processing"
	}
	v := Video{AuthorID: uid(c), Title: in.Title, Description: in.Description, MediaURL: in.MediaURL, CoverURL: in.CoverURL, Status: status}
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockOwnedMedia(tx, uid(c), in.MediaURL, "video"); err != nil {
			return err
		}
		if in.CoverURL != "" {
			if err := lockOwnedMedia(tx, uid(c), in.CoverURL, "image"); err != nil {
				return err
			}
		}
		if err := tx.Create(&v).Error; err != nil {
			return err
		}
		if status == "processing" {
			return tx.Create(&VideoProcessJob{VideoID: v.ID, SourceURL: v.MediaURL, AvailableAt: time.Now().UTC()}).Error
		}
		return s.enqueueFeedFanout(tx, v)
	}); err != nil {
		if errors.Is(err, errMediaNotOwned) {
			respondError(c, 400, "video or cover must be uploaded by this account")
			return
		}
		respondError(c, 500, "could not publish video")
		return
	}
	if status == "published" {
		s.invalidateFeed(c.Request.Context())
	}
	s.DB.Preload("Author").First(&v, v.ID)
	c.JSON(201, v)
}

var errMediaNotOwned = errors.New("media is not registered to this account")
var errVideoProcessorDisabled = errors.New("video processing is disabled")

// All writers of a media URL lock its registration before creating a reference.
// The asset reaper takes the same lock, so it cannot delete a selected upload.
func lockOwnedMedia(tx *gorm.DB, ownerID uint, url, kind string) error {
	var asset MediaAsset
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("url = ? AND owner_id = ? AND kind = ?", url, ownerID, kind).First(&asset).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return errMediaNotOwned
	}
	return err
}

func (s *Server) getVideo(c *gin.Context) {
	var v Video
	if s.DB.Preload("Author").First(&v, c.Param("id")).Error != nil || (v.Status != "published" && v.AuthorID != uid(c)) || v.Status == "deleted" {
		respondError(c, 404, "video not found")
		return
	}
	if v.Status == "failed" {
		var job VideoProcessJob
		err := s.DB.Select("failure_code").Where("video_id = ? AND dead_at IS NOT NULL", v.ID).Order("id DESC").First(&job).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			respondError(c, 500, "could not load video status")
			return
		}
		v.ProcessingError = publicProcessingCode(job.FailureCode)
	}
	c.JSON(200, v)
}

func publicProcessingCode(code string) string {
	switch code {
	case "duration_limit", "resolution_limit", "invalid_duration", "missing_video", "invalid_media":
		return code
	default:
		return "processing_failed"
	}
}

func (s *Server) deleteVideo(c *gin.Context) {
	var v Video
	if s.DB.Where("id = ? AND status <> ?", c.Param("id"), "deleted").First(&v).Error != nil {
		respondError(c, 404, "video not found")
		return
	}
	if v.AuthorID != uid(c) {
		respondError(c, 403, "only the author can delete this video")
		return
	}
	res := s.DB.Model(&Video{}).Where("id = ? AND author_id = ? AND status <> ?", v.ID, uid(c), "deleted").Update("status", "deleted")
	if res.Error != nil {
		respondError(c, 500, "could not delete video")
		return
	}
	if res.RowsAffected == 0 {
		respondError(c, 404, "video not found")
		return
	}
	s.invalidateFeed(c.Request.Context())
	c.Status(204)
}

func (s *Server) updateVideo(c *gin.Context) {
	var v Video
	if s.DB.Where("id = ? AND status <> ?", c.Param("id"), "deleted").First(&v).Error != nil {
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
		updates["media_url"] = *in.MediaURL
	}
	if in.CoverURL != nil {
		updates["cover_url"] = *in.CoverURL
	}
	if len(updates) == 0 {
		respondError(c, 400, "no video fields supplied")
		return
	}
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		if in.MediaURL != nil {
			if err := lockOwnedMedia(tx, uid(c), *in.MediaURL, "video"); err != nil {
				return err
			}
		}
		if in.CoverURL != nil && *in.CoverURL != "" {
			if err := lockOwnedMedia(tx, uid(c), *in.CoverURL, "image"); err != nil {
				return err
			}
		}
		var current Video
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND author_id = ? AND status <> ?", v.ID, uid(c), "deleted").First(&current).Error; err != nil {
			return err
		}
		if in.MediaURL != nil && s.ffmpegBin == "" && (current.Status == "failed" || current.Status == "processing") {
			return errVideoProcessorDisabled
		}
		needProcessing := s.ffmpegBin != "" && in.MediaURL != nil && (*in.MediaURL != current.MediaURL || current.Status == "failed")
		if in.MediaURL != nil && (*in.MediaURL != current.MediaURL || needProcessing) {
			updates["duration_ms"] = 0
			updates["width"] = 0
			updates["height"] = 0
		}
		if needProcessing {
			updates["status"] = "processing"
			if in.CoverURL == nil {
				updates["cover_url"] = ""
			}
		}
		if err := tx.Model(&current).Updates(updates).Error; err != nil {
			return err
		}
		if needProcessing {
			return tx.Create(&VideoProcessJob{VideoID: v.ID, SourceURL: *in.MediaURL, AvailableAt: time.Now().UTC()}).Error
		}
		return nil
	}); err != nil {
		if errors.Is(err, errMediaNotOwned) {
			respondError(c, 400, "video or cover must be uploaded by this account")
			return
		}
		if errors.Is(err, errVideoProcessorDisabled) {
			respondError(c, http.StatusConflict, "video processing is unavailable; retry later or contact the administrator")
			return
		}
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
	authorID, _ := strconv.ParseUint(c.Param("id"), 10, 32)
	q := s.DB.Preload("Author").Where("author_id = ?", authorID)
	if uid(c) == uint(authorID) && authorID > 0 {
		q = q.Where("status <> ?", "deleted")
	} else {
		q = q.Where("status = ?", "published")
	}
	if err := q.Order("created_at DESC").Limit(50).Find(&items).Error; err != nil {
		respondError(c, 500, "could not load user videos")
		return
	}
	if uid(c) == uint(authorID) && authorID > 0 {
		failedIDs := make([]uint, 0)
		for _, video := range items {
			if video.Status == "failed" {
				failedIDs = append(failedIDs, video.ID)
			}
		}
		if len(failedIDs) > 0 {
			var jobs []VideoProcessJob
			if err := s.DB.Select("video_id", "failure_code").Where("video_id IN ? AND dead_at IS NOT NULL", failedIDs).Order("id DESC").Find(&jobs).Error; err != nil {
				respondError(c, 500, "could not load video status")
				return
			}
			codes := make(map[uint]string, len(jobs))
			for _, job := range jobs {
				if _, exists := codes[job.VideoID]; !exists {
					codes[job.VideoID] = job.FailureCode
				}
			}
			for i := range items {
				if items[i].Status == "failed" {
					items[i].ProcessingError = publicProcessingCode(codes[items[i].ID])
				}
			}
		}
	}
	c.JSON(200, gin.H{"items": items})
}
