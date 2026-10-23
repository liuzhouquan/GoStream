package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReconcileOrphanMediaProtectsReferencesAndRecentFiles(t *testing.T) {
	s, h, db := testServer(t)
	ownerID, _ := registerTestUser(t, h, "reconcile_owner")
	now := time.Now().UTC()
	old := now.Add(-25 * time.Hour)
	write := func(id int, age time.Time) (string, string) {
		t.Helper()
		name := fmt.Sprintf("%032x.mp4", id)
		path := filepath.Join(s.MediaDir, name)
		if err := os.WriteFile(path, []byte("media"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, age, age); err != nil {
			t.Fatal(err)
		}
		return path, "/media/" + name
	}
	orphan, _ := write(1, old)
	assetPath, assetURL := write(2, old)
	videoPath, videoURL := write(3, old)
	avatarPath, avatarURL := write(4, old)
	recentPath, _ := write(5, now.Add(-time.Hour))
	jobPath, jobURL := write(7, old)
	if err := db.Create(&MediaAsset{OwnerID: ownerID, URL: assetURL, Kind: "video", Size: 5}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Video{AuthorID: ownerID, Title: "legacy", MediaURL: videoURL, Status: "deleted"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&User{}).Where("id = ?", ownerID).Update("avatar_url", avatarURL).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&VideoProcessJob{VideoID: 99999, SourceURL: jobURL, AvailableAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(s.MediaDir, "manually-added.mp4")
	if err := os.WriteFile(unknown, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(unknown, old, old); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(s.MediaDir, fmt.Sprintf("%032x.mp4", 6))
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	removed, err := s.ReconcileOrphanMedia(context.Background(), now)
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v, want 1", removed, err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan remains: %v", err)
	}
	for _, path := range []string{assetPath, videoPath, avatarPath, jobPath, recentPath, unknown, link, target} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("protected file %s: %v", path, err)
		}
	}
	if again, err := s.ReconcileOrphanMedia(context.Background(), now); err != nil || again != 0 {
		t.Fatalf("repeat removed=%d err=%v", again, err)
	}
}

func TestReconcileOrphanMediaStopsOnDatabaseError(t *testing.T) {
	s, _, db := testServer(t)
	name := fmt.Sprintf("%032x.mp4", 1)
	path := filepath.Join(s.MediaDir, name)
	if err := os.WriteFile(path, []byte("orphan"), 0644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.ReconcileOrphanMedia(context.Background(), time.Now()); err == nil || removed != 0 {
		t.Fatalf("database error: removed=%d err=%v", removed, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("orphan removed after database error: %v", err)
	}
}

func TestReconcileOrphanMediaAcrossBatches(t *testing.T) {
	s, h, db := testServer(t)
	ownerID, _ := registerTestUser(t, h, "reconcile_batches")
	old := time.Now().Add(-25 * time.Hour)
	protected := fmt.Sprintf("%032x.mp4", mediaReconcileBatch+1)
	if err := db.Create(&MediaAsset{OwnerID: ownerID, URL: "/media/" + protected, Kind: "video", Size: 1}).Error; err != nil {
		t.Fatal(err)
	}
	for id := 1; id <= mediaReconcileBatch+1; id++ {
		path := filepath.Join(s.MediaDir, fmt.Sprintf("%032x.mp4", id))
		if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := s.ReconcileOrphanMedia(context.Background(), time.Now())
	if err != nil || removed != mediaReconcileBatch {
		t.Fatalf("removed=%d err=%v, want %d", removed, err, mediaReconcileBatch)
	}
	if _, err := os.Stat(filepath.Join(s.MediaDir, protected)); err != nil {
		t.Fatalf("asset in second batch was removed: %v", err)
	}
}

func TestReconcileUnusedAssetsProtectsReferencesAndRecentFiles(t *testing.T) {
	s, h, db := testServer(t)
	ownerID, token := registerTestUser(t, h, "unused_assets")
	now := time.Now().UTC()
	old := now.Add(-mediaAssetGrace - time.Hour)
	add := func(id int, kind string, created, modified time.Time) (string, string) {
		t.Helper()
		name := fmt.Sprintf("%032x.mp4", id)
		if kind == "image" {
			name = fmt.Sprintf("%032x.png", id)
		}
		path, url := filepath.Join(s.MediaDir, name), "/media/"+name
		if err := os.WriteFile(path, []byte("media"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&MediaAsset{OwnerID: ownerID, URL: url, Kind: kind, Size: 5, CreatedAt: created}).Error; err != nil {
			t.Fatal(err)
		}
		return path, url
	}
	unusedPath, unusedURL := add(1, "video", old, old)
	missingPath, missingURL := add(2, "video", old, old)
	if err := os.Remove(missingPath); err != nil {
		t.Fatal(err)
	}
	videoPath, videoURL := add(3, "video", old, old)
	coverPath, coverURL := add(4, "image", old, old)
	avatarPath, avatarURL := add(5, "image", old, old)
	jobPath, jobURL := add(6, "video", old, old)
	recentAssetPath, recentAssetURL := add(7, "video", now.Add(-time.Hour), old)
	recentFilePath, recentFileURL := add(8, "video", old, now.Add(-time.Hour))
	if err := db.Create(&Video{AuthorID: ownerID, Title: "retained", MediaURL: videoURL, CoverURL: coverURL, Status: "deleted"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&User{}).Where("id = ?", ownerID).Update("avatar_url", avatarURL).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&VideoProcessJob{VideoID: 99999, SourceURL: jobURL, AvailableAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if removed, err := s.ReconcileUnusedAssets(context.Background(), now); err != nil || removed != 2 {
		t.Fatalf("removed=%d err=%v, want 2", removed, err)
	}
	if _, err := os.Lstat(unusedPath); !os.IsNotExist(err) {
		t.Fatalf("unused file remains: %v", err)
	}
	for _, url := range []string{unusedURL, missingURL} {
		var count int64
		if err := db.Model(&MediaAsset{}).Where("url = ?", url).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("registration %s: count=%d err=%v", url, count, err)
		}
	}
	for _, path := range []string{videoPath, coverPath, avatarPath, jobPath, recentAssetPath, recentFilePath} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("protected file %s: %v", path, err)
		}
	}
	for _, url := range []string{videoURL, coverURL, avatarURL, jobURL, recentAssetURL, recentFileURL} {
		var count int64
		if err := db.Model(&MediaAsset{}).Where("url = ?", url).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("protected registration %s: count=%d err=%v", url, count, err)
		}
	}
	if response := request(t, h, "POST", "/api/videos", token, map[string]string{"title": "too late", "media_url": unusedURL}); response.Code != 400 {
		t.Fatalf("removed media accepted: %d %s", response.Code, response.Body.String())
	}
	if again, err := s.ReconcileUnusedAssets(context.Background(), now); err != nil || again != 0 {
		t.Fatalf("repeat removed=%d err=%v", again, err)
	}
}

func TestReconcileUnusedAssetsSkipsUnsafeNamesAndSymlinks(t *testing.T) {
	s, h, db := testServer(t)
	ownerID, _ := registerTestUser(t, h, "unsafe_assets")
	now := time.Now().UTC()
	old := now.Add(-mediaAssetGrace - time.Hour)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	linkName := fmt.Sprintf("%032x.mp4", 1)
	link := filepath.Join(s.MediaDir, linkName)
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{"/media/" + linkName, "/media/../outside.mp4", "https://example.com/asset.mp4"} {
		if err := db.Create(&MediaAsset{OwnerID: ownerID, URL: url, Kind: "video", Size: 4, CreatedAt: old}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if removed, err := s.ReconcileUnusedAssets(context.Background(), now); err != nil || removed != 0 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	for _, path := range []string{outside, link} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("protected path %s: %v", path, err)
		}
	}
}

func TestReconcileUnusedAssetsAfterVideoAndAvatarReplacement(t *testing.T) {
	s, h, db := testServer(t)
	ownerID, token := registerTestUser(t, h, "asset_replacement")
	now := time.Now().UTC()
	old := now.Add(-mediaAssetGrace - time.Hour)
	add := func(id int, kind string, created time.Time) (string, string) {
		t.Helper()
		ext := ".mp4"
		if kind == "image" {
			ext = ".png"
		}
		name := fmt.Sprintf("%032x%s", id, ext)
		path, url := filepath.Join(s.MediaDir, name), "/media/"+name
		if err := os.WriteFile(path, []byte("media"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&MediaAsset{OwnerID: ownerID, URL: url, Kind: kind, Size: 5, CreatedAt: created}).Error; err != nil {
			t.Fatal(err)
		}
		return path, url
	}
	oldVideoPath, oldVideoURL := add(1, "video", old)
	newVideoPath, newVideoURL := add(2, "video", now)
	oldAvatarPath, oldAvatarURL := add(3, "image", old)
	newAvatarPath, newAvatarURL := add(4, "image", now)
	created := request(t, h, http.MethodPost, "/api/videos", token, map[string]string{"title": "replaceable", "media_url": oldVideoURL})
	if created.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", created.Code, created.Body.String())
	}
	var video Video
	if err := json.Unmarshal(created.Body.Bytes(), &video); err != nil {
		t.Fatal(err)
	}
	if response := request(t, h, http.MethodPatch, "/api/users/me", token, map[string]string{"avatar_url": oldAvatarURL}); response.Code != http.StatusOK {
		t.Fatalf("set avatar: %d %s", response.Code, response.Body.String())
	}
	if removed, err := s.ReconcileUnusedAssets(context.Background(), now); err != nil || removed != 0 {
		t.Fatalf("still referenced: removed=%d err=%v", removed, err)
	}
	videoPath := fmt.Sprintf("/api/videos/%d", video.ID)
	if response := request(t, h, http.MethodPatch, videoPath, token, map[string]string{"media_url": newVideoURL}); response.Code != http.StatusOK {
		t.Fatalf("replace video: %d %s", response.Code, response.Body.String())
	}
	if response := request(t, h, http.MethodPatch, "/api/users/me", token, map[string]string{"avatar_url": newAvatarURL}); response.Code != http.StatusOK {
		t.Fatalf("replace avatar: %d %s", response.Code, response.Body.String())
	}
	if removed, err := s.ReconcileUnusedAssets(context.Background(), now); err != nil || removed != 2 {
		t.Fatalf("released assets: removed=%d err=%v", removed, err)
	}
	for _, path := range []string{oldVideoPath, oldAvatarPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("released file remains: %v", err)
		}
	}
	for _, path := range []string{newVideoPath, newAvatarPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("replacement file missing: %v", err)
		}
	}
}

func TestReconcileUnusedAssetsAcrossBatchesAndDatabaseError(t *testing.T) {
	s, h, db := testServer(t)
	ownerID, _ := registerTestUser(t, h, "asset_batches")
	now := time.Now().UTC()
	old := now.Add(-mediaAssetGrace - time.Hour)
	for id := 1; id <= mediaReconcileBatch+1; id++ {
		name := fmt.Sprintf("%032x.mp4", id)
		if err := db.Create(&MediaAsset{OwnerID: ownerID, URL: "/media/" + name, Kind: "video", CreatedAt: old}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if removed, err := s.ReconcileUnusedAssets(context.Background(), now); err != nil || removed != mediaReconcileBatch+1 {
		t.Fatalf("removed=%d err=%v, want %d", removed, err, mediaReconcileBatch+1)
	}
	name := fmt.Sprintf("%032x.mp4", mediaReconcileBatch+2)
	path := filepath.Join(s.MediaDir, name)
	if err := os.WriteFile(path, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&MediaAsset{OwnerID: ownerID, URL: "/media/" + name, Kind: "video", CreatedAt: old}).Error; err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.ReconcileUnusedAssets(context.Background(), now); err == nil || removed != 0 {
		t.Fatalf("database error: removed=%d err=%v", removed, err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("file removed after database error: %v", err)
	}
}
