package platform

import (
	"context"
	"fmt"
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
