package platform

import (
	"context"
	"errors"
	"sync"
	"time"
)

var errUploadLeaseLost = errors.New("upload session lost lease")

const uploadCompletionLeaseDuration = 5 * time.Minute

// The heartbeat owns deadline/err until done closes. stop waits for it before
// the final transaction or failure cleanup uses the latest lease generation.
type uploadCompletionLease struct {
	stopOnce sync.Once
	stopCh   chan struct{}
	done     chan struct{}
	deadline time.Time
	err      error
}

func (s *Server) keepUploadCompletionLease(ctx context.Context, id string, until time.Time) *uploadCompletionLease {
	lease := &uploadCompletionLease{stopCh: make(chan struct{}), done: make(chan struct{}), deadline: until}
	interval := s.uploadRenewEvery
	if interval <= 0 {
		interval = uploadCompletionLeaseDuration / 3
	}
	go func() {
		defer close(lease.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-lease.stopCh:
				return
			case <-ctx.Done():
				lease.err = ctx.Err()
				return
			case <-ticker.C:
				now := time.Now().UTC()
				next := now.Add(uploadCompletionLeaseDuration).Truncate(time.Millisecond)
				renewCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				updated := s.DB.WithContext(renewCtx).Model(&UploadSession{}).
					Where("id = ? AND status = ? AND lease_until = ? AND lease_until > ?", id, "completing", lease.deadline, now).
					Update("lease_until", next)
				cancel()
				if updated.Error != nil {
					lease.err = updated.Error
					return
				}
				if updated.RowsAffected != 1 {
					lease.err = errUploadLeaseLost
					return
				}
				lease.deadline = next
			}
		}
	}()
	return lease
}

func (l *uploadCompletionLease) stop() (time.Time, error) {
	l.stopOnce.Do(func() { close(l.stopCh) })
	<-l.done
	return l.deadline, l.err
}
