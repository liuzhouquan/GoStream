package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const uploadChunkSize int64 = 4 << 20
const maxMediaSize int64 = 250 << 20

func (s *Server) uploadDir(id string) string {
	return filepath.Join(filepath.Clean(s.MediaDir)+".chunks", id)
}

func (s *Server) createUploadSession(c *gin.Context) {
	var in struct {
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
		SHA256   string `json:"sha256"`
	}
	if c.ShouldBindJSON(&in) != nil || in.Size < 1 || in.Size > maxMediaSize {
		respondError(c, 400, "filename and size of 1-250 MiB are required")
		return
	}
	ext := strings.ToLower(filepath.Ext(in.Filename))
	if !map[string]bool{".mp4": true, ".mov": true, ".webm": true, ".jpg": true, ".jpeg": true, ".png": true, ".webp": true}[ext] {
		respondError(c, 400, "unsupported media extension")
		return
	}
	if in.SHA256 != "" {
		decoded, err := hex.DecodeString(in.SHA256)
		if err != nil || len(decoded) != sha256.Size {
			respondError(c, 400, "sha256 must be a 64-character hex digest")
			return
		}
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		respondError(c, 500, "could not create upload session")
		return
	}
	session := UploadSession{
		ID: hex.EncodeToString(nonce[:]), OwnerID: uid(c), Ext: ext, Size: in.Size,
		SHA256: strings.ToLower(in.SHA256), ChunkSize: uploadChunkSize,
		Status: "open", CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
	}
	if err := s.DB.Create(&session).Error; err != nil {
		respondError(c, 500, "could not create upload session")
		return
	}
	c.JSON(201, gin.H{"id": session.ID, "size": session.Size, "chunk_size": session.ChunkSize, "expires_at": session.ExpiresAt})
}

func (s *Server) ownedUpload(c *gin.Context) (UploadSession, bool) {
	id := c.Param("id")
	if len(id) != 32 {
		respondError(c, 404, "upload session not found")
		return UploadSession{}, false
	}
	if _, err := hex.DecodeString(id); err != nil {
		respondError(c, 404, "upload session not found")
		return UploadSession{}, false
	}
	var session UploadSession
	if err := s.DB.Where("id = ? AND owner_id = ?", id, uid(c)).First(&session).Error; err != nil {
		respondError(c, 404, "upload session not found")
		return UploadSession{}, false
	}
	return session, true
}

func (s *Server) getUploadSession(c *gin.Context) {
	session, ok := s.ownedUpload(c)
	if !ok {
		return
	}
	var chunks []UploadChunk
	if err := s.DB.Where("session_id = ?", session.ID).Order("`index` ASC").Find(&chunks).Error; err != nil {
		respondError(c, 500, "could not load upload session")
		return
	}
	indices := make([]int, 0, len(chunks))
	for _, chunk := range chunks {
		indices = append(indices, chunk.Index)
	}
	url := ""
	if session.Status == "completed" {
		url = "/media/" + session.ID + session.Ext
	}
	c.JSON(200, gin.H{"id": session.ID, "size": session.Size, "chunk_size": session.ChunkSize,
		"status": session.Status, "expires_at": session.ExpiresAt, "uploaded_chunks": indices, "url": url})
}

func (s *Server) putUploadChunk(c *gin.Context) {
	session, ok := s.ownedUpload(c)
	if !ok {
		return
	}
	if session.Status != "open" || !time.Now().UTC().Before(session.ExpiresAt) {
		respondError(c, 409, "upload session is not open")
		return
	}
	index, err := strconv.Atoi(c.Param("index"))
	count := int((session.Size + session.ChunkSize - 1) / session.ChunkSize)
	if err != nil || index < 0 || index >= count {
		respondError(c, 400, "invalid chunk index")
		return
	}
	expected := session.Size - int64(index)*session.ChunkSize
	if expected > session.ChunkSize {
		expected = session.ChunkSize
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, expected+1)
	data, err := io.ReadAll(c.Request.Body)
	if err != nil || int64(len(data)) != expected {
		respondError(c, 400, "chunk size does not match session")
		return
	}
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	var existing UploadChunk
	err = s.DB.Where("session_id = ? AND `index` = ?", session.ID, index).First(&existing).Error
	if s.objectMedia != nil {
		s.putObjectChunk(c, session, index, expected, data, hash, existing, err)
		return
	}
	if err == nil {
		if existing.SHA256 != hash || existing.Size != expected {
			respondError(c, 409, "chunk already uploaded with different content")
			return
		}
		if _, err := os.Stat(filepath.Join(s.uploadDir(session.ID), fmt.Sprintf("%03d.part", index))); err == nil {
			c.JSON(200, gin.H{"index": index, "sha256": hash, "uploaded": true})
			return
		}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		respondError(c, 500, "could not inspect upload chunk")
		return
	}
	dir := s.uploadDir(session.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		respondError(c, 500, "media storage unavailable")
		return
	}
	tmp, err := os.CreateTemp(dir, "chunk-*")
	if err != nil {
		respondError(c, 500, "media storage unavailable")
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		respondError(c, 500, "could not save chunk")
		return
	}
	partPath := filepath.Join(dir, fmt.Sprintf("%03d.part", index))
	if err := os.Link(tmpName, partPath); err != nil {
		if !os.IsExist(err) {
			respondError(c, 500, "could not save chunk")
			return
		}
		stored, readErr := os.ReadFile(partPath)
		if readErr != nil || !bytesEqualHash(stored, hash) {
			respondError(c, 409, "chunk already uploaded with different content")
			return
		}
	}
	res := s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&UploadChunk{SessionID: session.ID, Index: index, Size: expected, SHA256: hash})
	if res.Error != nil {
		respondError(c, 500, "could not register chunk")
		return
	}
	if res.RowsAffected == 0 {
		if err := s.DB.Where("session_id = ? AND `index` = ?", session.ID, index).First(&existing).Error; err != nil || existing.SHA256 != hash {
			respondError(c, 409, "chunk already uploaded with different content")
			return
		}
	}
	c.JSON(200, gin.H{"index": index, "sha256": hash, "uploaded": true})
}

func bytesEqualHash(data []byte, want string) bool {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == want
}

func (s *Server) completeUploadSession(c *gin.Context) {
	session, ok := s.ownedUpload(c)
	if !ok {
		return
	}
	url := "/media/" + session.ID + session.Ext
	if session.Status == "completed" {
		c.JSON(200, gin.H{"url": url, "size": session.Size})
		return
	}
	now := time.Now().UTC()
	leaseUntil := now.Add(5 * time.Minute).Truncate(time.Millisecond)
	claim := s.DB.Model(&UploadSession{}).
		Where("id = ? AND owner_id = ? AND expires_at > ? AND (status = ? OR (status = ? AND (lease_until IS NULL OR lease_until < ?)))", session.ID, uid(c), now, "open", "completing", now).
		Updates(map[string]any{"status": "completing", "lease_until": leaseUntil})
	if claim.Error != nil {
		respondError(c, 500, "could not claim upload session")
		return
	}
	if claim.RowsAffected == 0 {
		respondError(c, 409, "upload session is busy or expired")
		return
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = s.DB.Model(&UploadSession{}).Where("id = ? AND status = ? AND lease_until = ?", session.ID, "completing", leaseUntil).Updates(map[string]any{"status": "open", "lease_until": nil}).Error
		}
	}()
	if s.objectMedia != nil {
		s.completeObjectUploadSession(c, session, url, leaseUntil, &succeeded)
		return
	}
	var chunks []UploadChunk
	if err := s.DB.Where("session_id = ?", session.ID).Order("`index` ASC").Find(&chunks).Error; err != nil {
		respondError(c, 500, "could not load chunks")
		return
	}
	count := int((session.Size + session.ChunkSize - 1) / session.ChunkSize)
	if len(chunks) != count {
		respondError(c, 409, "upload is incomplete")
		return
	}
	dir := s.uploadDir(session.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		respondError(c, 500, "media storage unavailable")
		return
	}
	tmp, err := os.CreateTemp(dir, "assembled-*")
	if err != nil {
		respondError(c, 500, "media storage unavailable")
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	allHash := sha256.New()
	var total int64
	for i, chunk := range chunks {
		if chunk.Index != i {
			_ = tmp.Close()
			respondError(c, 409, "upload has missing chunks")
			return
		}
		part, err := os.Open(filepath.Join(dir, fmt.Sprintf("%03d.part", i)))
		if err != nil {
			_ = tmp.Close()
			respondError(c, 409, "upload chunk is missing")
			return
		}
		partHash := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(tmp, allHash, partHash), part)
		_ = part.Close()
		if copyErr != nil || n != chunk.Size || hex.EncodeToString(partHash.Sum(nil)) != chunk.SHA256 {
			_ = tmp.Close()
			respondError(c, 409, "upload chunk failed integrity check")
			return
		}
		total += n
	}
	if err := tmp.Close(); err != nil || total != session.Size {
		respondError(c, 500, "could not assemble upload")
		return
	}
	hash := hex.EncodeToString(allHash.Sum(nil))
	if session.SHA256 != "" && hash != session.SHA256 {
		respondError(c, 409, "upload sha256 mismatch")
		return
	}
	assembled, err := os.Open(tmpName)
	if err != nil {
		respondError(c, 500, "could not inspect upload")
		return
	}
	header := make([]byte, 512)
	nHeader, _ := io.ReadFull(assembled, header)
	_ = assembled.Close()
	kind := mediaKind(session.Ext, header[:nHeader])
	if kind == "" {
		respondError(c, 400, "file content does not match a supported media type")
		return
	}
	if kind == "image" && !validImageFile(tmpName, session.Ext) {
		respondError(c, 400, "invalid image file")
		return
	}
	if err := os.MkdirAll(s.MediaDir, 0755); err != nil {
		respondError(c, 500, "media storage unavailable")
		return
	}
	finalPath := filepath.Join(s.MediaDir, session.ID+session.Ext)
	linked := false
	if err := os.Link(tmpName, finalPath); err == nil {
		linked = true
	} else if !os.IsExist(err) {
		respondError(c, 500, "could not finalize upload")
		return
	} else {
		stored, readErr := os.Open(finalPath)
		if readErr != nil {
			respondError(c, 500, "could not inspect existing upload")
			return
		}
		storedHash := sha256.New()
		storedSize, readErr := io.Copy(storedHash, stored)
		_ = stored.Close()
		if readErr != nil || storedSize != total || hex.EncodeToString(storedHash.Sum(nil)) != hash {
			respondError(c, 409, "upload final file conflicts with session")
			return
		}
	}
	if err := s.registerCompletedUpload(c.Request.Context(), uid(c), session, url, kind, total, leaseUntil); err != nil {
		if linked {
			_ = os.Remove(finalPath)
		}
		respondError(c, 500, "could not register completed upload")
		return
	}
	succeeded = true
	if os.RemoveAll(dir) == nil && s.DB.Where("session_id = ?", session.ID).Delete(&UploadChunk{}).Error == nil {
		_ = s.DB.Model(&UploadSession{}).Where("id = ?", session.ID).Update("cleaned_at", time.Now().UTC()).Error
	}
	c.JSON(200, gin.H{"url": url, "size": total, "sha256": hash})
}

func (s *Server) registerCompletedUpload(ctx context.Context, ownerID uint, session UploadSession, url, kind string, total int64, leaseUntil time.Time) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&MediaAsset{OwnerID: ownerID, URL: url, Kind: kind, Size: total})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			var existing MediaAsset
			if err := tx.Where("url = ?", url).First(&existing).Error; err != nil {
				return err
			}
			if existing.OwnerID != ownerID || existing.Size != total || existing.Kind != kind {
				return errors.New("existing asset conflicts with upload")
			}
		}
		updated := tx.Model(&UploadSession{}).Where("id = ? AND status = ? AND lease_until = ?", session.ID, "completing", leaseUntil).Updates(map[string]any{"status": "completed", "lease_until": nil})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return errors.New("upload session lost completion lease")
		}
		return nil
	})
}

// CleanupExpiredUploads removes partial files after their 24-hour resume window.
// Completed sessions remain in the database so completion retries stay idempotent.
func (s *Server) CleanupExpiredUploads(ctx context.Context) (int, error) {
	now := time.Now().UTC()
	var sessions []UploadSession
	if err := s.DB.WithContext(ctx).Where("(expires_at <= ? AND (status = ? OR (status = ? AND (lease_until IS NULL OR lease_until < ?)))) OR (status = ? AND cleaned_at IS NULL AND created_at <= ?)",
		now, "open", "completing", now, "completed", now.Add(-time.Hour)).Order("created_at ASC").Limit(100).Find(&sessions).Error; err != nil {
		return 0, err
	}
	cleaned := 0
	for _, session := range sessions {
		if err := ctx.Err(); err != nil {
			return cleaned, err
		}
		if s.objectMedia != nil {
			if err := s.objectMedia.removePrefix(ctx, "chunks/"+session.ID+"/"); err != nil {
				return cleaned, err
			}
		} else {
			if err := os.RemoveAll(s.uploadDir(session.ID)); err != nil {
				return cleaned, err
			}
		}
		if err := s.DB.WithContext(ctx).Where("session_id = ?", session.ID).Delete(&UploadChunk{}).Error; err != nil {
			return cleaned, err
		}
		if session.Status != "completed" {
			if err := s.DB.WithContext(ctx).Where("id = ? AND status <> ?", session.ID, "completed").Delete(&UploadSession{}).Error; err != nil {
				return cleaned, err
			}
		} else {
			if err := s.DB.WithContext(ctx).Model(&UploadSession{}).Where("id = ?", session.ID).Update("cleaned_at", now).Error; err != nil {
				return cleaned, err
			}
		}
		cleaned++
	}
	return cleaned, nil
}

func (s *Server) RunUploadCleanup(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		if _, err := s.CleanupExpiredUploads(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("upload cleanup failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
