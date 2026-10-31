package platform

import (
	"slices"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type receiptInterval struct{ first, last uint }

func receiptIntervals(ids []uint) []receiptInterval {
	ordered := slices.Clone(ids)
	slices.Sort(ordered)
	var runs []receiptInterval
	for _, id := range ordered {
		if len(runs) > 0 {
			last := &runs[len(runs)-1]
			if id <= last.last {
				continue
			}
			if id-last.last == 1 {
				last.last = id
				continue
			}
		}
		runs = append(runs, receiptInterval{id, id})
	}
	return runs
}

func (r receiptInterval) adjacentWindow() receiptInterval {
	if r.first > 0 {
		r.first--
	}
	if r.last < ^uint(0) {
		r.last++
	}
	return r
}

// The user row is already locked by the caller. No current-unread test or
// MAX(message_id) watermark is used: ranges must remain exact sets of IDs.
func storeDeviceReceipts(tx *gorm.DB, userID uint, deviceID string, ids []uint) (int64, error) {
	runs := receiptIntervals(ids)
	conditions := make([]string, 0, len(runs))
	args := make([]any, 0, len(runs)*2)
	for _, r := range runs {
		conditions = append(conditions, "(first_message_id <= ? AND last_message_id >= ?)")
		args = append(args, r.last, r.first)
	}
	var covered []MessageDeviceAckRange
	if err := tx.Where("user_id = ? AND device_id = ?", userID, deviceID).
		Where(strings.Join(conditions, " OR "), args...).Find(&covered).Error; err != nil {
		return 0, err
	}
	var inserts []MessageDeviceAck
	for _, id := range ids {
		read := false
		for _, r := range covered {
			if id >= r.FirstMessageID && id <= r.LastMessageID {
				read = true
				break
			}
		}
		if !read {
			inserts = append(inserts, MessageDeviceAck{UserID: userID, DeviceID: deviceID, MessageID: id})
		}
	}
	var created int64
	if len(inserts) > 0 {
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&inserts)
		if result.Error != nil {
			return 0, result.Error
		}
		created = result.RowsAffected
	}
	conditions, args = nil, nil
	for _, r := range runs {
		w := r.adjacentWindow()
		conditions = append(conditions, "message_id BETWEEN ? AND ?")
		args = append(args, w.first, w.last)
	}
	var exact []MessageDeviceAck
	if err := tx.Where("user_id = ? AND device_id = ?", userID, deviceID).
		Where(strings.Join(conditions, " OR "), args...).Order("message_id ASC").Find(&exact).Error; err != nil {
		return 0, err
	}
	for start := 0; start < len(exact); {
		end := start + 1
		for end < len(exact) && exact[end].MessageID-exact[end-1].MessageID == 1 {
			end++
		}
		r := receiptInterval{exact[start].MessageID, exact[end-1].MessageID}
		w := r.adjacentWindow()
		var neighbors []MessageDeviceAckRange
		if err := tx.Where("user_id = ? AND device_id = ? AND first_message_id <= ? AND last_message_id >= ?", userID, deviceID, w.last, w.first).Find(&neighbors).Error; err != nil {
			return 0, err
		}
		if end-start >= 2 || len(neighbors) > 0 {
			for _, n := range neighbors {
				r.first = min(r.first, n.FirstMessageID)
				r.last = max(r.last, n.LastMessageID)
			}
			if len(neighbors) > 0 {
				neighborIDs := make([]uint, len(neighbors))
				for i, n := range neighbors {
					neighborIDs[i] = n.ID
				}
				if err := tx.Where("id IN ?", neighborIDs).Delete(&MessageDeviceAckRange{}).Error; err != nil {
					return 0, err
				}
			}
			if err := tx.Create(&MessageDeviceAckRange{UserID: userID, DeviceID: deviceID, FirstMessageID: r.first, LastMessageID: r.last}).Error; err != nil {
				return 0, err
			}
			exactIDs := make([]uint, end-start)
			for i := start; i < end; i++ {
				exactIDs[i-start] = exact[i].ID
			}
			if err := tx.Where("id IN ?", exactIDs).Delete(&MessageDeviceAck{}).Error; err != nil {
				return 0, err
			}
		}
		start = end
	}
	return created, nil
}
