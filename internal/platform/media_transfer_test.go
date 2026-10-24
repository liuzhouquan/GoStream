package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMediaTransferRejectsUnsafeKeysAndFiles(t *testing.T) {
	for _, key := range []string{"media/../file.mp4", "media/../../etc/passwd", "chunks/../x", "other/file.mp4"} {
		if validMediaObjectKey(key) {
			t.Fatalf("accepted unsafe key %q", key)
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "data"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := hashRegularFile(filepath.Join(dir, "link")); err == nil {
		t.Fatal("symlink accepted as backup source")
	}
	index := mediaBackupIndex{Version: 1, Objects: []mediaBackupEntry{{Key: "media/../file.mp4", Size: 1, SHA256: strings.Repeat("a", 64)}}}
	data, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, mediaBackupManifest), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreMediaObjects(context.Background(), &objectMediaStore{}, dir); err == nil {
		t.Fatal("unsafe manifest entry accepted")
	}
}

// Opt in with the same disposable MinIO endpoint as TestMinIOMediaAcrossInstances.
func TestMinIOMediaTransfer(t *testing.T) {
	endpoint := os.Getenv("GOSTREAM_TEST_MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("set GOSTREAM_TEST_MINIO_ENDPOINT to a disposable MinIO server")
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	makeStore := func(suffix string) *objectMediaStore {
		t.Helper()
		bucket := "gostream-transfer-" + hex.EncodeToString(nonce[:]) + "-" + suffix
		store, err := NewMinIOMediaStore(ctx, endpoint, os.Getenv("GOSTREAM_TEST_MINIO_ACCESS_KEY"), os.Getenv("GOSTREAM_TEST_MINIO_SECRET_KEY"), bucket, false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			for _, prefix := range []string{"media/", "chunks/"} {
				if err := store.removePrefix(cleanCtx, prefix); err != nil {
					t.Errorf("remove %s: %v", prefix, err)
				}
			}
			if err := store.client.RemoveBucket(cleanCtx, bucket); err != nil {
				t.Errorf("remove bucket: %v", err)
			}
		})
		return store
	}
	sourceStore := makeStore("source")
	destStore := makeStore("dest")
	emptyStore := makeStore("empty")
	_, _, db := testServer(t)
	user := User{Username: "transfer_author", PasswordHash: "test", Nickname: "Author"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	mediaDir := t.TempDir()
	videoName, _ := randomMediaName(".mp4")
	coverName, _ := randomMediaName(".jpg")
	videoURL, coverURL := "/media/"+videoName, "/media/"+coverName
	if err := os.WriteFile(filepath.Join(mediaDir, videoName), sampleMP4, 0600); err != nil {
		t.Fatal(err)
	}
	cover := []byte{0xff, 0xd8, 0xff, 0xd9}
	if err := os.WriteFile(filepath.Join(mediaDir, coverName), cover, 0600); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&MediaAsset{OwnerID: user.ID, URL: videoURL, Kind: "video", Size: int64(len(sampleMP4))}).Error; err != nil {
		t.Fatal(err)
	}
	video := Video{AuthorID: user.ID, Title: "Legacy media", MediaURL: videoURL, CoverURL: coverURL, Status: "published"}
	if err := db.Create(&video).Error; err != nil {
		t.Fatal(err)
	}
	staleName, err := randomMediaName(".mp4")
	if err != nil {
		t.Fatal(err)
	}
	done := time.Now().UTC()
	if err := db.Create(&VideoProcessJob{VideoID: video.ID, SourceURL: "/media/" + staleName, AvailableAt: done, DoneAt: &done}).Error; err != nil {
		t.Fatal(err)
	}
	chunkHash := sha256.Sum256(sampleMP4)
	activeID := strings.Repeat("b", 32)
	activeSession := UploadSession{ID: activeID, OwnerID: user.ID, Ext: ".mp4", Size: int64(len(sampleMP4)), ChunkSize: uploadChunkSize, Status: "open", CreatedAt: done, ExpiresAt: done.Add(time.Hour)}
	if err := db.Create(&activeSession).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&UploadChunk{SessionID: activeID, Index: 0, Size: int64(len(sampleMP4)), SHA256: hex.EncodeToString(chunkHash[:])}).Error; err != nil {
		t.Fatal(err)
	}
	chunkDir := filepath.Join(filepath.Clean(mediaDir)+".chunks", activeID)
	if err := os.MkdirAll(chunkDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chunkDir, "000.part"), sampleMP4, 0600); err != nil {
		t.Fatal(err)
	}
	activeChunkKey := chunkObjectKey(activeID, 0, hex.EncodeToString(chunkHash[:]))
	report, err := ImportLocalMedia(ctx, db, mediaDir, sourceStore)
	if err != nil || report.Objects != 3 || report.Copied != 3 {
		t.Fatalf("first import: report=%+v err=%v", report, err)
	}
	report, err = ImportLocalMedia(ctx, db, mediaDir, sourceStore)
	if err != nil || report.Existing != 3 || report.Copied != 0 {
		t.Fatalf("repeat import: report=%+v err=%v", report, err)
	}
	if err := verifyObject(ctx, sourceStore, activeChunkKey, int64(len(sampleMP4)), hex.EncodeToString(chunkHash[:])); err != nil {
		t.Fatalf("imported resumable chunk: %v", err)
	}
	chunkKey := chunkObjectKey(strings.Repeat("a", 32), 0, hex.EncodeToString(chunkHash[:]))
	if err := sourceStore.putBytes(ctx, chunkKey, sampleMP4); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(t.TempDir(), "snapshot")
	report, err = BackupMediaObjects(ctx, sourceStore, backupDir)
	if err != nil || report.Objects != 4 || report.Copied != 4 {
		t.Fatalf("backup: report=%+v err=%v", report, err)
	}
	if _, err := BackupMediaObjects(ctx, sourceStore, backupDir); err == nil {
		t.Fatal("existing backup directory was overwritten")
	}
	report, err = RestoreMediaObjects(ctx, destStore, backupDir)
	if err != nil || report.Objects != 4 || report.Copied != 4 {
		t.Fatalf("restore: report=%+v err=%v", report, err)
	}
	report, err = RestoreMediaObjects(ctx, destStore, backupDir)
	if err != nil || report.Existing != 4 || report.Copied != 0 {
		t.Fatalf("repeat restore: report=%+v err=%v", report, err)
	}
	extraName, err := randomMediaName(".mp4")
	if err != nil {
		t.Fatal(err)
	}
	if err := destStore.putBytes(ctx, "media/"+extraName, sampleMP4); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreMediaObjects(ctx, destStore, backupDir); err == nil {
		t.Fatal("restore accepted an extra destination object")
	}
	if err := destStore.remove(ctx, "media/"+extraName); err != nil {
		t.Fatal(err)
	}
	if err := verifyObject(ctx, destStore, "media/"+videoName, int64(len(sampleMP4)), hex.EncodeToString(chunkHash[:])); err != nil {
		t.Fatalf("restored published video: %v", err)
	}
	if err := verifyObject(ctx, destStore, activeChunkKey, int64(len(sampleMP4)), hex.EncodeToString(chunkHash[:])); err != nil {
		t.Fatalf("restored resumable chunk: %v", err)
	}
	if err := destStore.putBytes(ctx, "media/"+videoName, sampleMP4); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreMediaObjects(ctx, destStore, backupDir); err == nil {
		t.Fatal("restore accepted an existing object with wrong content type")
	}
	if err := os.WriteFile(filepath.Join(backupDir, "objects", filepath.FromSlash(chunkKey)), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreMediaObjects(ctx, emptyStore, backupDir); err == nil {
		t.Fatal("corrupt backup was accepted")
	}
	if _, err := emptyStore.stat(ctx, "media/"+videoName); !objectMissing(err) {
		t.Fatalf("preflight wrote into empty bucket: %v", err)
	}
	if err := os.WriteFile(filepath.Join(chunkDir, "000.part"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportLocalMedia(ctx, db, mediaDir, emptyStore); err == nil {
		t.Fatal("chunk content differing from SQL was accepted")
	}
	if _, err := emptyStore.stat(ctx, activeChunkKey); !objectMissing(err) {
		t.Fatalf("invalid local chunk was copied: %v", err)
	}
	if err := sourceStore.putBytes(ctx, "media/"+videoName, []byte("wrong content")); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportLocalMedia(ctx, db, mediaDir, sourceStore); err == nil {
		t.Fatal("existing conflicting object was accepted")
	}
	if err := os.Remove(filepath.Join(mediaDir, coverName)); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportLocalMedia(ctx, db, mediaDir, destStore); err == nil {
		t.Fatal("missing referenced local file was accepted")
	}
	missingBucket := "gostream-transfer-" + hex.EncodeToString(nonce[:]) + "-absent"
	if _, err := OpenExistingMinIOMediaStore(ctx, endpoint, os.Getenv("GOSTREAM_TEST_MINIO_ACCESS_KEY"), os.Getenv("GOSTREAM_TEST_MINIO_SECRET_KEY"), missingBucket, false); err == nil {
		t.Fatal("backup opened a nonexistent bucket")
	}
}
