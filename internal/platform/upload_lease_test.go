package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
)

// Pause a real completion request after it has claimed the session and loaded
// its chunks. The resume window expires while assembly is still in progress.
func TestUploadCompletionKeepsLeaseDuringAssembly(t *testing.T) {
	t.Run("without_cleanup", func(t *testing.T) { exerciseUploadAssemblyExpiry(t, false) })
	t.Run("with_cleanup", func(t *testing.T) { exerciseUploadAssemblyExpiry(t, true) })
}

func TestUploadCleanupRecoversAfterDatabaseFailure(t *testing.T) {
	s, _, db := testServer(t)
	past := time.Now().UTC().Add(-time.Minute)
	session := UploadSession{ID: "234567890abcdef1234567890abcdef1", OwnerID: 1, Ext: ".mp4", Size: 1, ChunkSize: uploadChunkSize, Status: "open", ExpiresAt: past}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&UploadChunk{SessionID: session.ID, Index: 0, Size: 1, SHA256: "fixture"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.uploadDir(session.ID), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.uploadDir(session.ID), "000.part"), []byte{1}, 0600); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("injected cleanup transaction failure")
	var fail atomic.Bool
	fail.Store(true)
	const callback = "test:fail_upload_cleanup"
	if err := db.Callback().Delete().Before("gorm:delete").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "upload_chunks" && fail.Load() {
			tx.AddError(failure)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Delete().Remove(callback) })
	if cleaned, err := s.CleanupExpiredUploads(context.Background()); cleaned != 0 || !errors.Is(err, failure) {
		t.Fatalf("cleanup failure: count=%d err=%v", cleaned, err)
	}
	if err := db.First(&session, "id = ?", session.ID).Error; err != nil || session.Status != "cleaning" || session.LeaseUntil == nil {
		t.Fatalf("cleanup claim was not retained: %+v err=%v", session, err)
	}
	fail.Store(false)
	if cleaned, err := s.CleanupExpiredUploads(context.Background()); cleaned != 0 || err != nil {
		t.Fatalf("another cleaner stole a live claim: count=%d err=%v", cleaned, err)
	}
	if err := db.Model(&UploadSession{}).Where("id = ?", session.ID).Update("lease_until", past).Error; err != nil {
		t.Fatal(err)
	}
	if cleaned, err := s.CleanupExpiredUploads(context.Background()); cleaned != 1 || err != nil {
		t.Fatalf("expired cleanup claim was not recovered: count=%d err=%v", cleaned, err)
	}
	var remaining int64
	if err := db.Model(&UploadChunk{}).Where("session_id = ?", session.ID).Count(&remaining).Error; err != nil || remaining != 0 {
		t.Fatalf("chunk registrations remain: count=%d err=%v", remaining, err)
	}
	if err := db.Model(&UploadSession{}).Where("id = ?", session.ID).Count(&remaining).Error; err != nil || remaining != 0 {
		t.Fatalf("session remains: count=%d err=%v", remaining, err)
	}
	if cleaned, err := s.CleanupExpiredUploads(context.Background()); cleaned != 0 || err != nil {
		t.Fatalf("cleanup retry was not idempotent: count=%d err=%v", cleaned, err)
	}
}

func TestUploadCompletionHeartbeatStopsOnFailure(t *testing.T) {
	for _, kind := range []string{"canceled", "lease_lost", "database_closed"} {
		t.Run(kind, func(t *testing.T) {
			s, _, db := testServer(t)
			s.uploadRenewEvery = time.Millisecond
			until := time.Now().UTC().Add(uploadCompletionLeaseDuration).Truncate(time.Millisecond)
			session := UploadSession{ID: "34567890abcdef1234567890abcdef12", OwnerID: 1, Ext: ".mp4", Size: 1, ChunkSize: uploadChunkSize, Status: "completing", LeaseUntil: &until, ExpiresAt: until}
			if err := db.Create(&session).Error; err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "lease_lost" {
				if err := db.Model(&UploadSession{}).Where("id = ?", session.ID).Update("lease_until", until.Add(time.Minute)).Error; err != nil {
					t.Fatal(err)
				}
			}
			if kind == "database_closed" {
				sqlDB, err := db.DB()
				if err != nil {
					t.Fatal(err)
				}
				if err := sqlDB.Close(); err != nil {
					t.Fatal(err)
				}
			}
			lease := s.keepUploadCompletionLease(ctx, session.ID, until)
			if kind == "canceled" {
				cancel()
			}
			select {
			case <-lease.done:
			case <-time.After(3 * time.Second):
				t.Fatal("heartbeat did not exit after failure")
			}
			_, err := lease.stop()
			if err == nil || (kind == "canceled" && !errors.Is(err, context.Canceled)) || (kind == "lease_lost" && !errors.Is(err, errUploadLeaseLost)) {
				t.Fatalf("heartbeat error for %s: %v", kind, err)
			}
			// Repeated shutdown and failed finalization must be safe.
			if _, again := lease.stop(); again != err {
				t.Fatalf("shutdown changed the failure: %v / %v", err, again)
			}
			if err := s.registerCompletedUpload(context.Background(), 1, session, "/media/failure.mp4", "video", 1, lease); err == nil {
				t.Fatal("failed lease registered an asset")
			}
			if kind != "database_closed" {
				var assets int64
				if err := db.Model(&MediaAsset{}).Count(&assets).Error; err != nil || assets != 0 {
					t.Fatalf("failed lease left assets: count=%d err=%v", assets, err)
				}
			}
		})
	}
}

// A late worker may have linked the shared final file before another worker
// takes over and commits it. Its failed finalization must preserve that file.
func TestUploadCompletionLeaseLossKeepsCommittedMedia(t *testing.T) {
	s, h, db := testServer(t)
	_, token := registerTestUser(t, h, "lateassembler")
	created := request(t, h, http.MethodPost, "/api/uploads/sessions", token, map[string]any{"filename": "clip.mp4", "size": len(sampleMP4)})
	var session UploadSession
	if created.Code != http.StatusCreated || json.Unmarshal(created.Body.Bytes(), &session) != nil {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	if r := putTestChunk(h, token, session.ID, 0, sampleMP4); r.Code != http.StatusOK {
		t.Fatalf("chunk: %d %s", r.Code, r.Body.String())
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var blocked atomic.Bool
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	const callback = "test:pause_upload_registration"
	if err := db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*MediaAsset); ok && blocked.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Create().Remove(callback) })
	done := make(chan *httptest.ResponseRecorder, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done <- request(t, h, http.MethodPost, "/api/uploads/sessions/"+session.ID+"/complete", token, nil)
	}()
	t.Cleanup(func() { unblock(); <-finished })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old completion did not reach registration")
	}
	if err := db.Model(&UploadSession{}).Where("id = ?", session.ID).Update("lease_until", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	newer := request(t, h, http.MethodPost, "/api/uploads/sessions/"+session.ID+"/complete", token, nil)
	unblock()
	older := <-done
	if newer.Code != http.StatusOK || older.Code != http.StatusInternalServerError {
		t.Fatalf("handoff results: new=%d %s old=%d %s", newer.Code, newer.Body.String(), older.Code, older.Body.String())
	}
	if err := db.First(&session, "id = ?", session.ID).Error; err != nil || session.Status != "completed" {
		t.Fatalf("committed session: %+v err=%v", session, err)
	}
	if data, err := os.ReadFile(filepath.Join(s.MediaDir, session.ID+".mp4")); err != nil || string(data) != string(sampleMP4) {
		t.Fatalf("late completion removed committed media: bytes=%d err=%v", len(data), err)
	}
}

func exerciseUploadAssemblyExpiry(t *testing.T, cleanup bool) {
	t.Helper()
	s, h, db := testServer(t)
	s.uploadRenewEvery = 10 * time.Millisecond
	_, token := registerTestUser(t, h, "slowassembler")
	created := request(t, h, http.MethodPost, "/api/uploads/sessions", token, map[string]any{"filename": "clip.mp4", "size": len(sampleMP4)})
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	var session UploadSession
	if err := json.Unmarshal(created.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	if r := putTestChunk(h, token, session.ID, 0, sampleMP4); r.Code != http.StatusOK {
		t.Fatalf("chunk: %d %s", r.Code, r.Body.String())
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce, pauseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	const callback = "test:pause_upload_assembly"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*[]UploadChunk); ok {
			pauseOnce.Do(func() { close(entered); <-release })
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	done := make(chan *httptest.ResponseRecorder, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done <- request(t, h, http.MethodPost, "/api/uploads/sessions/"+session.ID+"/complete", token, nil)
	}()
	t.Cleanup(func() { unblock(); <-finished })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("completion did not reach assembly")
	}
	var claimed UploadSession
	if err := db.First(&claimed, "id = ?", session.ID).Error; err != nil || claimed.LeaseUntil == nil {
		t.Fatalf("initial completion lease: %+v err=%v", claimed, err)
	}
	cutoff := claimed.LeaseUntil.Add(time.Millisecond)
	if err := db.Model(&UploadSession{}).Where("id = ?", session.ID).Update("expires_at", cutoff.Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	// Observe a real heartbeat, then advance only the cleaner's cutoff past
	// the original lease. This avoids relying on subsecond lease expiry races.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var current UploadSession
		if err := db.First(&current, "id = ?", session.ID).Error; err != nil {
			t.Fatal(err)
		}
		if current.LeaseUntil != nil && current.LeaseUntil.After(cutoff) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	var cleaned int
	var cleanErr error
	if cleanup {
		cleaned, cleanErr = s.cleanupExpiredUploadsAt(context.Background(), cutoff)
	}
	unblock()
	var result *httptest.ResponseRecorder
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("completion did not finish after resuming")
	}
	if cleanErr != nil || cleaned != 0 {
		t.Errorf("cleanup removed an active completion: cleaned=%d err=%v", cleaned, cleanErr)
	}
	if result.Code != http.StatusOK {
		t.Fatalf("active completion failed: %d %s", result.Code, result.Body.String())
	}
	if err := db.First(&session, "id = ?", session.ID).Error; err != nil || session.Status != "completed" {
		t.Fatalf("completed session: %+v err=%v", session, err)
	}
	if data, err := os.ReadFile(filepath.Join(s.MediaDir, session.ID+".mp4")); err != nil || string(data) != string(sampleMP4) {
		t.Fatalf("final media differs: bytes=%d err=%v", len(data), err)
	}
}

func TestUploadCleanupRechecksCompletionLease(t *testing.T) {
	s, _, db := testServer(t)
	now := time.Now().UTC()
	past := now.Add(-time.Minute)
	session := UploadSession{ID: "1234567890abcdef1234567890abcdef", OwnerID: 1, Ext: ".mp4", Size: 1, ChunkSize: uploadChunkSize, Status: "completing", LeaseUntil: &past, ExpiresAt: past}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.uploadDir(session.ID), 0755); err != nil {
		t.Fatal(err)
	}
	chunkPath := filepath.Join(s.uploadDir(session.ID), "000.part")
	if err := os.WriteFile(chunkPath, []byte{1}, 0600); err != nil {
		t.Fatal(err)
	}
	const callback = "test:renew_after_cleanup_selection"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*[]UploadSession); ok {
			if err := db.Model(&UploadSession{}).Where("id = ?", session.ID).Update("lease_until", now.Add(time.Hour)).Error; err != nil {
				tx.AddError(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	if cleaned, err := s.CleanupExpiredUploads(context.Background()); err != nil || cleaned != 0 {
		t.Fatalf("stale cleanup selection ignored renewed lease: cleaned=%d err=%v", cleaned, err)
	}
	if _, err := os.Stat(chunkPath); err != nil {
		t.Fatalf("renewed completion lost its chunk: %v", err)
	}
}
