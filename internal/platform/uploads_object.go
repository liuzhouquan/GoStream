package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// A content-addressed key means two API instances can race to upload the same
// chunk index without overwriting each other's bytes. The unique SQL row picks
// one hash; unselected objects are removed when the session expires.
func chunkObjectKey(sessionID string, index int, hash string) string {
	return fmt.Sprintf("chunks/%s/%03d/%s", sessionID, index, hash)
}

func (s *Server) putObjectChunk(c *gin.Context, session UploadSession, index int, expected int64, data []byte, hash string, existing UploadChunk, lookupErr error) {
	if lookupErr == nil && (existing.SHA256 != hash || existing.Size != expected) {
		respondError(c, 409, "chunk already uploaded with different content")
		return
	}
	if lookupErr != nil && !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
		respondError(c, 500, "could not inspect upload chunk")
		return
	}
	key := chunkObjectKey(session.ID, index, hash)
	info, err := s.objectMedia.stat(c.Request.Context(), key)
	if err != nil && !objectMissing(err) {
		respondError(c, 503, "media storage unavailable")
		return
	}
	if err != nil || info.Size != expected {
		if err := s.objectMedia.putBytes(c.Request.Context(), key, data); err != nil {
			respondError(c, 503, "media storage unavailable")
			return
		}
	}
	res := s.DB.WithContext(c.Request.Context()).Clauses(clause.OnConflict{DoNothing: true}).Create(&UploadChunk{SessionID: session.ID, Index: index, Size: expected, SHA256: hash})
	if res.Error != nil {
		respondError(c, 500, "could not register chunk")
		return
	}
	if res.RowsAffected == 0 {
		if err := s.DB.WithContext(c.Request.Context()).Where("session_id = ? AND `index` = ?", session.ID, index).First(&existing).Error; err != nil || existing.SHA256 != hash || existing.Size != expected {
			respondError(c, 409, "chunk already uploaded with different content")
			return
		}
	}
	c.JSON(200, gin.H{"index": index, "sha256": hash, "uploaded": true})
}

func (s *Server) completeObjectUploadSession(c *gin.Context, session UploadSession, url string, leaseUntil time.Time, succeeded *bool) {
	var chunks []UploadChunk
	if err := s.DB.WithContext(c.Request.Context()).Where("session_id = ?", session.ID).Order("`index` ASC").Find(&chunks).Error; err != nil {
		respondError(c, 500, "could not load chunks")
		return
	}
	count := int((session.Size + session.ChunkSize - 1) / session.ChunkSize)
	if len(chunks) != count {
		respondError(c, 409, "upload is incomplete")
		return
	}
	tmp, err := os.CreateTemp("", "gostream-assembled-*")
	if err != nil {
		respondError(c, 500, "media storage unavailable")
		return
	}
	defer os.Remove(tmp.Name())
	allHash := sha256.New()
	var total int64
	for index, chunk := range chunks {
		if chunk.Index != index {
			_ = tmp.Close()
			respondError(c, 409, "upload has missing chunks")
			return
		}
		part, err := s.objectMedia.get(c.Request.Context(), chunkObjectKey(session.ID, index, chunk.SHA256))
		if err != nil {
			_ = tmp.Close()
			respondError(c, 503, "media storage unavailable")
			return
		}
		partHash := sha256.New()
		n, readErr := io.Copy(io.MultiWriter(tmp, allHash, partHash), part)
		closeErr := part.Close()
		if objectMissing(readErr) {
			_ = tmp.Close()
			respondError(c, 409, "upload chunk is missing")
			return
		}
		if readErr != nil || closeErr != nil {
			_ = tmp.Close()
			respondError(c, 503, "media storage unavailable")
			return
		}
		if n != chunk.Size || hex.EncodeToString(partHash.Sum(nil)) != chunk.SHA256 {
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
	assembled, err := os.Open(tmp.Name())
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
	if kind == "image" && !validImageFile(tmp.Name(), session.Ext) {
		respondError(c, 400, "invalid image file")
		return
	}
	if err := s.objectMedia.putFile(c.Request.Context(), "media/"+session.ID+session.Ext, tmp.Name()); err != nil {
		respondError(c, 503, "media storage unavailable")
		return
	}
	if err := s.registerCompletedUpload(c.Request.Context(), uid(c), session, url, kind, total, leaseUntil); err != nil {
		// Keep the uploaded object for a retry. The orphan worker reclaims it
		// after a grace period if the transaction never commits.
		respondError(c, 500, "could not register completed upload")
		return
	}
	*succeeded = true
	cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if s.objectMedia.removePrefix(cleanCtx, "chunks/"+session.ID+"/") == nil &&
		s.DB.WithContext(cleanCtx).Where("session_id = ?", session.ID).Delete(&UploadChunk{}).Error == nil {
		_ = s.DB.WithContext(cleanCtx).Model(&UploadSession{}).Where("id = ?", session.ID).Update("cleaned_at", time.Now().UTC()).Error
	}
	c.JSON(200, gin.H{"url": url, "size": total, "sha256": hash})
}
