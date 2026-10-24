package platform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// objectMediaStore keeps the bucket private. All public reads go through the
// same-origin /media route so the browser needs no S3 credentials or signed URL.
type objectMediaStore struct {
	client *minio.Client
	bucket string
}

func NewMinIOMediaStore(ctx context.Context, endpoint, accessKey, secretKey, bucket string, secure bool) (*objectMediaStore, error) {
	if endpoint == "" || accessKey == "" || secretKey == "" || bucket == "" {
		return nil, errors.New("MinIO endpoint, credentials, and bucket are required")
	}
	client, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(accessKey, secretKey, ""), Secure: secure, MaxRetries: 2})
	if err != nil {
		return nil, fmt.Errorf("configure MinIO: %w", err)
	}
	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("inspect MinIO bucket: %w", err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
			response := minio.ToErrorResponse(err)
			if response.Code != "BucketAlreadyOwnedByYou" {
				return nil, fmt.Errorf("create MinIO bucket: %w", err)
			}
		}
	}
	return &objectMediaStore{client: client, bucket: bucket}, nil
}

func (s *Server) UseObjectMedia(store *objectMediaStore) { s.objectMedia = store }

func (m *objectMediaStore) putBytes(ctx context.Context, key string, data []byte) error {
	_, err := m.client.PutObject(ctx, m.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{ContentType: "application/octet-stream"})
	return err
}

func (m *objectMediaStore) putFile(ctx context.Context, key, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	contentType := mime.TypeByExtension(filepath.Ext(key))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err = m.client.PutObject(ctx, m.bucket, key, f, info.Size(), minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (m *objectMediaStore) get(ctx context.Context, key string) (*minio.Object, error) {
	return m.client.GetObject(ctx, m.bucket, key, minio.GetObjectOptions{})
}

func (m *objectMediaStore) stat(ctx context.Context, key string) (minio.ObjectInfo, error) {
	return m.client.StatObject(ctx, m.bucket, key, minio.StatObjectOptions{})
}

func (m *objectMediaStore) remove(ctx context.Context, key string) error {
	return m.client.RemoveObject(ctx, m.bucket, key, minio.RemoveObjectOptions{})
}

func (m *objectMediaStore) removePrefix(ctx context.Context, prefix string) error {
	listCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for object := range m.client.ListObjects(listCtx, m.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if object.Err != nil {
			return object.Err
		}
		if err := m.remove(ctx, object.Key); err != nil {
			return err
		}
	}
	return nil
}

func (m *objectMediaStore) removeBestEffort(key string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = m.remove(ctx, key)
}

func objectMissing(err error) bool {
	if err == nil {
		return false
	}
	switch minio.ToErrorResponse(err).Code {
	case "NoSuchKey", "NoSuchObject", "NotFound":
		return true
	}
	return false
}

func (s *Server) serveObjectMedia(c *gin.Context) {
	name := c.Param("name")
	if !generatedMediaName.MatchString(name) {
		respondError(c, http.StatusNotFound, "media not found")
		return
	}
	object, err := s.objectMedia.get(c.Request.Context(), "media/"+name)
	if err != nil {
		respondError(c, http.StatusServiceUnavailable, "media storage unavailable")
		return
	}
	defer object.Close()
	info, err := object.Stat()
	if objectMissing(err) {
		respondError(c, http.StatusNotFound, "media not found")
		return
	}
	if err != nil {
		respondError(c, http.StatusServiceUnavailable, "media storage unavailable")
		return
	}
	if info.ContentType != "" {
		c.Header("Content-Type", info.ContentType)
	}
	c.Header("Cache-Control", "public, max-age=86400")
	http.ServeContent(c.Writer, c.Request, name, info.LastModified, object)
}
