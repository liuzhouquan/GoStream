package platform

import (
	"context"
	"database/sql"

	"gorm.io/gorm"
)

const directUnreadBatch = 256

func pendingDirectCandidates(db *gorm.DB, userID uint, deviceID string, after uint, limit int) *gorm.DB {
	q := db.Model(&ChatMessage{}).Where("chat_messages.recipient_id = ? AND chat_messages.acked_at IS NULL AND chat_messages.id > ?", userID, after)
	if deviceID != "" {
		q = q.Joins("LEFT JOIN message_device_acks ON message_device_acks.message_id = chat_messages.id AND message_device_acks.user_id = ? AND message_device_acks.device_id = ?", userID, deviceID).Where("message_device_acks.id IS NULL").Select("chat_messages.*")
	}
	return q.Order("chat_messages.id ASC").Limit(limit)
}

// Scan bounded, ordered message/range pages in one snapshot. A correlated
// containment subquery scans a device's intervals again for each message on
// MySQL. Constant range bounds here let its composite index seek directly.
func (s *Server) loadPendingDirectMessages(ctx context.Context, userID uint, deviceID string, after uint, limit int) ([]ChatMessage, error) {
	rows := make([]ChatMessage, 0, limit)
	db := s.DB.WithContext(ctx)
	if deviceID == "" {
		err := pendingDirectCandidates(db, userID, deviceID, after, limit).Find(&rows).Error
		return rows, err
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		cursor := after
		for len(rows) < limit {
			var candidates []ChatMessage
			if err := pendingDirectCandidates(tx, userID, deviceID, cursor, directUnreadBatch).Find(&candidates).Error; err != nil {
				return err
			}
			if len(candidates) == 0 {
				return nil
			}
			first, last := candidates[0].ID, candidates[len(candidates)-1].ID
			var previous, following []MessageDeviceAckRange
			scope := func() *gorm.DB { return tx.Where("user_id = ? AND device_id = ?", userID, deviceID) }
			// Nonoverlapping intervals mean only the nearest predecessor can cover first.
			if err := scope().Where("first_message_id <= ?", first).Order("first_message_id DESC").Limit(1).Find(&previous).Error; err != nil {
				return err
			}
			if err := scope().Where("first_message_id > ? AND first_message_id <= ?", first, last).Order("first_message_id ASC").Limit(directUnreadBatch).Find(&following).Error; err != nil {
				return err
			}
			boundary := last
			if len(following) == directUnreadBatch {
				// Global ACKs may hide arbitrarily many messages between two candidates.
				// Resume at the last known interval rather than loading all intervals or
				// treating any candidates beyond that boundary as unread.
				boundary = min(boundary, following[len(following)-1].LastMessageID)
			}
			ranges := append(previous, following...)
			r := 0
			for _, message := range candidates {
				if message.ID > boundary {
					break
				}
				for r < len(ranges) && ranges[r].LastMessageID < message.ID {
					r++
				}
				if r < len(ranges) && ranges[r].FirstMessageID <= message.ID {
					// All integer IDs through the interval end were individually ACKed;
					// jumping over this exact set cannot hide a late-committing gap.
					cursor = max(cursor, ranges[r].LastMessageID)
					continue
				}
				rows = append(rows, message)
				cursor = max(cursor, message.ID)
				if len(rows) == limit {
					return nil
				}
			}
			cursor = max(cursor, boundary)
			if len(candidates) < directUnreadBatch && cursor >= last {
				return nil
			}
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	return rows, nil
}
