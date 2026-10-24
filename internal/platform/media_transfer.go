package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"gorm.io/gorm"
)

const mediaBackupManifest = "manifest.json"

var chunkBackupKey = regexp.MustCompile(`^chunks/[0-9a-f]{32}/[0-9]{3}/[0-9a-f]{64}$`)
var sha256Digest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var uploadSessionID = regexp.MustCompile(`^[0-9a-f]{32}$`)

type MediaTransferReport struct {
	Objects  int   `json:"objects"`
	Copied   int   `json:"copied"`
	Existing int   `json:"existing"`
	Bytes    int64 `json:"bytes"`
}

type mediaBackupEntry struct {
	Key         string `json:"key"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	ContentType string `json:"content_type"`
}

type mediaBackupIndex struct {
	Version int                `json:"version"`
	Objects []mediaBackupEntry `json:"objects"`
}

func validMediaObjectKey(key string) bool {
	if strings.HasPrefix(key, "media/") {
		return generatedMediaName.MatchString(strings.TrimPrefix(key, "media/"))
	}
	return chunkBackupKey.MatchString(key)
}

// ImportLocalMedia copies every app-managed URL and live resumable chunk
// referenced by SQL into MinIO.
// Run it with writers stopped; it never changes SQL rows or removes local files.
// Existing objects must match the local SHA-256, making interrupted runs safe.
func ImportLocalMedia(ctx context.Context, db *gorm.DB, mediaDir string, store *objectMediaStore) (MediaTransferReport, error) {
	var report MediaTransferReport
	if db == nil || store == nil {
		return report, errors.New("database and MinIO store are required")
	}
	queries := []struct {
		model  any
		column string
		where  string
	}{
		{&MediaAsset{}, "url", ""},
		{&Video{}, "media_url", ""},
		{&Video{}, "cover_url", ""},
		{&User{}, "avatar_url", ""},
		{&VideoProcessJob{}, "source_url", "done_at IS NULL AND dead_at IS NULL"},
	}
	type source struct {
		path string
		size int64
		hash string
	}
	unique := map[string]source{}
	for _, query := range queries {
		var urls []string
		selection := db.WithContext(ctx).Model(query.model)
		if query.where != "" {
			selection = selection.Where(query.where)
		}
		if err := selection.Pluck(query.column, &urls).Error; err != nil {
			return report, fmt.Errorf("read %s media references: %w", query.column, err)
		}
		for _, url := range urls {
			if url == "" {
				continue
			}
			if !strings.HasPrefix(url, "/media/") {
				return report, fmt.Errorf("unexpected media URL %q", url)
			}
			key := strings.TrimPrefix(url, "/")
			if !validMediaObjectKey(key) {
				return report, fmt.Errorf("unsafe media URL %q", url)
			}
			unique[key] = source{path: filepath.Join(mediaDir, strings.TrimPrefix(key, "media/"))}
		}
	}
	var sessions []UploadSession
	if err := db.WithContext(ctx).Where("status IN ? AND expires_at > ?", []string{"open", "completing"}, time.Now().UTC()).Find(&sessions).Error; err != nil {
		return report, fmt.Errorf("read live upload sessions: %w", err)
	}
	for _, session := range sessions {
		if !uploadSessionID.MatchString(session.ID) || session.ChunkSize <= 0 || session.Size <= 0 {
			return report, fmt.Errorf("invalid upload session %q", session.ID)
		}
		var chunks []UploadChunk
		if err := db.WithContext(ctx).Where("session_id = ?", session.ID).Find(&chunks).Error; err != nil {
			return report, fmt.Errorf("read upload chunks for %s: %w", session.ID, err)
		}
		count := (session.Size-1)/session.ChunkSize + 1
		for _, chunk := range chunks {
			if chunk.Index < 0 || int64(chunk.Index) >= count || chunk.Index > 999 || !sha256Digest.MatchString(chunk.SHA256) {
				return report, fmt.Errorf("invalid chunk for upload %s", session.ID)
			}
			expected := session.Size - int64(chunk.Index)*session.ChunkSize
			if expected > session.ChunkSize {
				expected = session.ChunkSize
			}
			if chunk.Size != expected {
				return report, fmt.Errorf("invalid size for upload %s chunk %d", session.ID, chunk.Index)
			}
			key := chunkObjectKey(session.ID, chunk.Index, chunk.SHA256)
			unique[key] = source{path: filepath.Join(filepath.Clean(mediaDir)+".chunks", session.ID, fmt.Sprintf("%03d.part", chunk.Index)), size: chunk.Size, hash: chunk.SHA256}
		}
	}
	keys := make([]string, 0, len(unique))
	for key := range unique {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	report.Objects = len(keys)
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		item := unique[key]
		localHash, localSize, err := hashRegularFile(item.path)
		if err != nil {
			return report, fmt.Errorf("inspect local %s: %w", key, err)
		}
		if item.hash != "" && (localSize != item.size || localHash != item.hash) {
			return report, fmt.Errorf("local chunk %s differs from SQL record", key)
		}
		_, err = store.stat(ctx, key)
		if err != nil && !objectMissing(err) {
			return report, fmt.Errorf("inspect object %s: %w", key, err)
		}
		if objectMissing(err) {
			if err := store.putFile(ctx, key, item.path); err != nil {
				return report, fmt.Errorf("copy %s: %w", key, err)
			}
			report.Copied++
			report.Bytes += localSize
		} else {
			report.Existing++
		}
		if err := verifyObject(ctx, store, key, localSize, localHash); err != nil {
			return report, fmt.Errorf("verify %s: %w", key, err)
		}
	}
	return report, nil
}

// BackupMediaObjects writes a complete private bucket snapshot to a new local
// directory. A manifest is renamed into place only after all bytes are saved.
func BackupMediaObjects(ctx context.Context, store *objectMediaStore, dir string) (MediaTransferReport, error) {
	var report MediaTransferReport
	if store == nil {
		return report, errors.New("MinIO store is required")
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return report, fmt.Errorf("create new backup directory: %w", err)
	}
	listCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var keys []string
	for object := range store.client.ListObjects(listCtx, store.bucket, minio.ListObjectsOptions{Recursive: true}) {
		if object.Err != nil {
			return report, object.Err
		}
		if !validMediaObjectKey(object.Key) {
			return report, fmt.Errorf("unexpected object key %q", object.Key)
		}
		keys = append(keys, object.Key)
	}
	sort.Strings(keys)
	index := mediaBackupIndex{Version: 1, Objects: make([]mediaBackupEntry, 0, len(keys))}
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		before, err := store.stat(ctx, key)
		if err != nil {
			return report, fmt.Errorf("stat %s: %w", key, err)
		}
		path := filepath.Join(dir, "objects", filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return report, err
		}
		file, err := os.OpenFile(path+".part", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return report, err
		}
		object, err := store.get(ctx, key)
		if err != nil {
			_ = file.Close()
			return report, err
		}
		hash := sha256.New()
		size, copyErr := io.Copy(io.MultiWriter(file, hash), object)
		objectErr := object.Close()
		syncErr := file.Sync()
		fileErr := file.Close()
		if copyErr != nil || objectErr != nil || syncErr != nil || fileErr != nil || size != before.Size {
			return report, fmt.Errorf("copy %s: read=%v close=%v sync=%v file=%v size=%d expected=%d", key, copyErr, objectErr, syncErr, fileErr, size, before.Size)
		}
		after, err := store.stat(ctx, key)
		if err != nil || after.ETag != before.ETag || after.Size != before.Size {
			return report, fmt.Errorf("object %s changed during backup: %v", key, err)
		}
		if err := os.Rename(path+".part", path); err != nil {
			return report, err
		}
		index.Objects = append(index.Objects, mediaBackupEntry{Key: key, Size: size, SHA256: hex.EncodeToString(hash.Sum(nil)), ContentType: before.ContentType})
		report.Objects++
		report.Copied++
		report.Bytes += size
	}
	manifest, err := os.OpenFile(filepath.Join(dir, mediaBackupManifest+".part"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return report, err
	}
	encodeErr := json.NewEncoder(manifest).Encode(index)
	syncErr := manifest.Sync()
	closeErr := manifest.Close()
	if encodeErr != nil || syncErr != nil || closeErr != nil {
		return report, fmt.Errorf("write backup manifest: encode=%v sync=%v close=%v", encodeErr, syncErr, closeErr)
	}
	if err := os.Rename(filepath.Join(dir, mediaBackupManifest+".part"), filepath.Join(dir, mediaBackupManifest)); err != nil {
		return report, err
	}
	return report, nil
}

// RestoreMediaObjects validates every local file before touching the bucket.
// Existing matching objects are left intact; a conflicting object stops restore.
func RestoreMediaObjects(ctx context.Context, store *objectMediaStore, dir string) (MediaTransferReport, error) {
	var report MediaTransferReport
	if store == nil {
		return report, errors.New("MinIO store is required")
	}
	manifest, err := os.Open(filepath.Join(dir, mediaBackupManifest))
	if err != nil {
		return report, err
	}
	defer manifest.Close()
	manifestInfo, err := manifest.Stat()
	if err != nil || manifestInfo.Size() > 64<<20 {
		return report, fmt.Errorf("invalid backup manifest size: %v", err)
	}
	var index mediaBackupIndex
	decoder := json.NewDecoder(io.LimitReader(manifest, 64<<20))
	if err := decoder.Decode(&index); err != nil {
		return report, fmt.Errorf("read backup manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return report, fmt.Errorf("backup manifest has trailing data: %v", err)
	}
	if index.Version != 1 {
		return report, fmt.Errorf("unsupported backup version %d", index.Version)
	}
	seen := map[string]bool{}
	for _, entry := range index.Objects {
		if !validMediaObjectKey(entry.Key) || !sha256Digest.MatchString(entry.SHA256) || entry.Size < 0 || seen[entry.Key] {
			return report, fmt.Errorf("invalid or duplicate backup entry %q", entry.Key)
		}
		seen[entry.Key] = true
		path := filepath.Join(dir, "objects", filepath.FromSlash(entry.Key))
		hash, size, err := hashRegularFile(path)
		if err != nil || hash != entry.SHA256 || size != entry.Size {
			return report, fmt.Errorf("backup file %s failed verification: %v", entry.Key, err)
		}
	}
	report.Objects = len(index.Objects)
	listCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for object := range store.client.ListObjects(listCtx, store.bucket, minio.ListObjectsOptions{Recursive: true}) {
		if object.Err != nil {
			return report, object.Err
		}
		if !seen[object.Key] {
			return report, fmt.Errorf("destination contains object absent from backup: %s", object.Key)
		}
	}
	missing := make([]mediaBackupEntry, 0, len(index.Objects))
	for _, entry := range index.Objects {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		info, err := store.stat(ctx, entry.Key)
		if err != nil && !objectMissing(err) {
			return report, err
		}
		if objectMissing(err) {
			missing = append(missing, entry)
			continue
		}
		if info.ContentType != entry.ContentType {
			return report, fmt.Errorf("conflicting content type for %s", entry.Key)
		}
		if err := verifyObject(ctx, store, entry.Key, entry.Size, entry.SHA256); err != nil {
			return report, fmt.Errorf("conflicting restored %s: %w", entry.Key, err)
		}
		report.Existing++
	}
	for _, entry := range missing {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		path := filepath.Join(dir, "objects", filepath.FromSlash(entry.Key))
		file, err := os.Open(path)
		if err != nil {
			return report, err
		}
		_, putErr := store.client.PutObject(ctx, store.bucket, entry.Key, file, entry.Size, minio.PutObjectOptions{ContentType: entry.ContentType})
		closeErr := file.Close()
		if putErr != nil || closeErr != nil {
			return report, fmt.Errorf("restore %s: put=%v close=%v", entry.Key, putErr, closeErr)
		}
		if err := verifyObject(ctx, store, entry.Key, entry.Size, entry.SHA256); err != nil {
			return report, fmt.Errorf("verify restored %s: %w", entry.Key, err)
		}
		info, err := store.stat(ctx, entry.Key)
		if err != nil || info.ContentType != entry.ContentType {
			return report, fmt.Errorf("verify restored content type %s: %v", entry.Key, err)
		}
		report.Copied++
		report.Bytes += entry.Size
	}
	return report, nil
}

func hashRegularFile(path string) (string, int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() {
		return "", 0, errors.New("source is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil || size != info.Size() {
		return "", 0, fmt.Errorf("source changed while hashing: read=%d expected=%d error=%v", size, info.Size(), err)
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}
