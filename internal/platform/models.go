package platform

import "time"

type User struct {
	ID           uint       `json:"id" gorm:"primaryKey"`
	Username     string     `json:"username" gorm:"size:32;uniqueIndex;not null"`
	PasswordHash string     `json:"-" gorm:"not null"`
	Nickname     string     `json:"nickname" gorm:"size:48;not null"`
	Bio          string     `json:"bio" gorm:"size:280"`
	AvatarURL    string     `json:"avatar_url" gorm:"size:512;index"`
	LastSeenAt   *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

type Video struct {
	ID              uint      `json:"id" gorm:"primaryKey;index:idx_video_feed_pull,priority:4,sort:desc"`
	AuthorID        uint      `json:"author_id" gorm:"index:idx_video_author,priority:1;not null"`
	Author          User      `json:"author,omitempty" gorm:"foreignKey:AuthorID"`
	Title           string    `json:"title" gorm:"size:120;not null"`
	Description     string    `json:"description" gorm:"size:2000"`
	MediaURL        string    `json:"media_url" gorm:"size:512;index;not null"`
	CoverURL        string    `json:"cover_url" gorm:"size:512;index"`
	Status          string    `json:"status" gorm:"size:16;index:idx_video_feed,priority:1;index:idx_video_feed_pull,priority:1;not null;default:published"`
	LikeCount       int64     `json:"like_count"`
	CommentCount    int64     `json:"comment_count"`
	FavoriteCount   int64     `json:"favorite_count"`
	PlayCount       int64     `json:"play_count"`
	DurationMS      int64     `json:"duration_ms" gorm:"not null;default:0"`
	Width           int       `json:"width" gorm:"not null;default:0"`
	Height          int       `json:"height" gorm:"not null;default:0"`
	ProcessingError string    `json:"processing_error,omitempty" gorm:"-"`
	CreatedAt       time.Time `json:"created_at" gorm:"index:idx_video_feed,priority:2,sort:desc;index:idx_video_author,priority:2,sort:desc;index:idx_video_feed_pull,priority:3,sort:desc"`
	FeedPull        bool      `json:"-" gorm:"index:idx_video_feed_pull,priority:2;not null;default:true"`
}

// VideoProcessJob survives restarts and lets another instance reclaim an expired lease.
type VideoProcessJob struct {
	ID          uint       `gorm:"primaryKey"`
	VideoID     uint       `gorm:"index;not null"`
	SourceURL   string     `gorm:"size:512;index;not null"`
	Attempts    int        `gorm:"not null;default:0"`
	AvailableAt time.Time  `gorm:"index;not null"`
	LeaseUntil  *time.Time `gorm:"index"`
	DoneAt      *time.Time `gorm:"index"`
	DeadAt      *time.Time `gorm:"index"`
	LastError   string     `gorm:"size:500"`
	FailureCode string     `gorm:"size:32;not null;default:''"`
	CreatedAt   time.Time
}

// MediaAsset records an uploaded file and the account allowed to publish it.
type MediaAsset struct {
	ID        uint      `gorm:"primaryKey"`
	OwnerID   uint      `gorm:"index;not null"`
	URL       string    `gorm:"size:512;uniqueIndex;not null"`
	Kind      string    `gorm:"size:16;not null"`
	Size      int64     `gorm:"not null"`
	CreatedAt time.Time `gorm:"index"`
}

type UploadSession struct {
	ID         string     `json:"id" gorm:"primaryKey;size:32"`
	OwnerID    uint       `json:"-" gorm:"index;not null"`
	Ext        string     `json:"-" gorm:"size:8;not null"`
	Size       int64      `json:"size" gorm:"not null"`
	SHA256     string     `json:"-" gorm:"size:64"`
	ChunkSize  int64      `json:"chunk_size" gorm:"not null"`
	Status     string     `json:"status" gorm:"size:16;not null;default:open"`
	LeaseUntil *time.Time `json:"-"`
	CleanedAt  *time.Time `json:"-"`
	ExpiresAt  time.Time  `json:"expires_at" gorm:"index"`
	CreatedAt  time.Time  `json:"created_at"`
}

type UploadChunk struct {
	ID        uint   `gorm:"primaryKey"`
	SessionID string `gorm:"uniqueIndex:ux_upload_chunk,priority:1;size:32;not null"`
	Index     int    `gorm:"uniqueIndex:ux_upload_chunk,priority:2;not null"`
	Size      int64  `gorm:"not null"`
	SHA256    string `gorm:"size:64;not null"`
}

type Follow struct {
	ID         uint      `gorm:"primaryKey"`
	FollowerID uint      `gorm:"uniqueIndex:ux_follow_pair,priority:1;index:idx_followers,priority:2;not null"`
	FolloweeID uint      `gorm:"uniqueIndex:ux_follow_pair,priority:2;index:idx_followers,priority:1;not null"`
	CreatedAt  time.Time `gorm:"index:idx_following,priority:2,sort:desc"`
}

// A completed job means the inbox has covered every follower present during
// its scan. New follows still read older videos from the author outbox.
type FeedFanoutJob struct {
	ID             uint       `gorm:"primaryKey;index:idx_feed_fanout_dispatch,priority:4"`
	VideoID        uint       `gorm:"uniqueIndex;not null"`
	AuthorID       uint       `gorm:"not null"`
	LastFollowerID uint       `gorm:"not null;default:0"`
	Attempts       int        `gorm:"not null;default:0"`
	AvailableAt    time.Time  `gorm:"not null;index:idx_feed_fanout_dispatch,priority:2"`
	LeaseUntil     *time.Time `gorm:"index:idx_feed_fanout_dispatch,priority:3"`
	DoneAt         *time.Time `gorm:"index:idx_feed_fanout_dispatch,priority:1"`
	LastError      string     `gorm:"size:500"`
	CreatedAt      time.Time
}

type FeedInbox struct {
	ID        uint      `gorm:"primaryKey"`
	UserID    uint      `gorm:"uniqueIndex:ux_feed_inbox,priority:1;index:idx_feed_inbox_user,priority:1;not null"`
	VideoID   uint      `gorm:"uniqueIndex:ux_feed_inbox,priority:2;index:idx_feed_inbox_user,priority:3,sort:desc;not null"`
	CreatedAt time.Time `gorm:"index:idx_feed_inbox_user,priority:2,sort:desc;not null"`
}

type VideoAction struct {
	ID        uint   `gorm:"primaryKey;index:idx_action_recent,priority:2,sort:desc"`
	UserID    uint   `gorm:"uniqueIndex:ux_action,priority:1;index:idx_action_recent,priority:1;not null"`
	VideoID   uint   `gorm:"uniqueIndex:ux_action,priority:2;index;not null"`
	Kind      string `gorm:"uniqueIndex:ux_action,priority:3;size:16;not null"`
	CreatedAt time.Time
}

type Comment struct {
	ID         uint      `json:"id" gorm:"primaryKey;index:idx_comment_thread,priority:4,sort:desc"`
	VideoID    uint      `json:"video_id" gorm:"index:idx_comment_page,priority:1;index:idx_comment_thread,priority:1;not null"`
	UserID     uint      `json:"user_id" gorm:"not null"`
	User       User      `json:"user,omitempty" gorm:"foreignKey:UserID"`
	ParentID   *uint     `json:"parent_id" gorm:"index:idx_comment_thread,priority:2"`
	Body       string    `json:"body" gorm:"size:2000;not null"`
	CreatedAt  time.Time `json:"created_at" gorm:"index:idx_comment_page,priority:2,sort:desc;index:idx_comment_thread,priority:3,sort:desc"`
	ReplyCount int64     `json:"reply_count,omitempty" gorm:"-"`
}

type Notification struct {
	ID        uint       `json:"id" gorm:"primaryKey;index:idx_notification_user_id,priority:2,sort:desc;index:idx_notification_unread,priority:3,sort:desc"`
	UserID    uint       `json:"-" gorm:"uniqueIndex:ux_notification_event,priority:1;index:idx_notification_page,priority:1;index:idx_notification_user_id,priority:1;index:idx_notification_unread,priority:1;not null"`
	EventKey  string     `json:"-" gorm:"uniqueIndex:ux_notification_event,priority:2;size:100;not null"`
	Kind      string     `json:"kind" gorm:"size:20;not null"`
	ActorID   uint       `json:"actor_id"`
	Actor     User       `json:"actor,omitempty" gorm:"foreignKey:ActorID"`
	VideoID   uint       `json:"video_id,omitempty"`
	Text      string     `json:"text" gorm:"size:280"`
	ReadAt    *time.Time `json:"read_at" gorm:"index:idx_notification_unread,priority:2"`
	CreatedAt time.Time  `json:"created_at" gorm:"index:idx_notification_page,priority:2,sort:desc"`
}

type ChatMessage struct {
	ID          uint       `json:"id" gorm:"primaryKey;index:idx_message_thread_id,priority:3,sort:desc"`
	ClientID    string     `json:"client_id" gorm:"uniqueIndex:ux_message_client,priority:1;size:80;not null"`
	SenderID    uint       `json:"sender_id" gorm:"uniqueIndex:ux_message_client,priority:2;index:idx_message_thread,priority:1;index:idx_message_thread_id,priority:1;not null"`
	RecipientID uint       `json:"recipient_id" gorm:"index:idx_message_thread,priority:2;index:idx_message_thread_id,priority:2;index:idx_message_unacked,priority:1;not null"`
	Body        string     `json:"body" gorm:"size:4000;not null"`
	CreatedAt   time.Time  `json:"created_at" gorm:"index:idx_message_thread,priority:3"`
	AckedAt     *time.Time `json:"acked_at" gorm:"index:idx_message_unacked,priority:2"`
}

// MessageDeviceAck records exact incoming messages displayed on one device.
// The legacy ChatMessage.AckedAt remains available to clients without a device ID.
type MessageDeviceAck struct {
	ID        uint      `gorm:"primaryKey"`
	UserID    uint      `gorm:"uniqueIndex:ux_message_device_ack,priority:1;not null"`
	DeviceID  string    `gorm:"uniqueIndex:ux_message_device_ack,priority:2;size:36;not null"`
	MessageID uint      `gorm:"uniqueIndex:ux_message_device_ack,priority:3;not null"`
	CreatedAt time.Time `gorm:"not null"`
}

// MessageDeviceAckRange contains only consecutive, explicitly acknowledged IDs.
// Missing IDs are never inferred to be read, including transactions committing late.
type MessageDeviceAckRange struct {
	ID             uint   `gorm:"primaryKey"`
	UserID         uint   `gorm:"uniqueIndex:ux_device_ack_range,priority:1;not null"`
	DeviceID       string `gorm:"uniqueIndex:ux_device_ack_range,priority:2;size:36;not null"`
	FirstMessageID uint   `gorm:"uniqueIndex:ux_device_ack_range,priority:3;not null"`
	LastMessageID  uint   `gorm:"not null"`
}

// MessageDevice bounds the number of device-specific read states per account.
type MessageDevice struct {
	ID         uint      `json:"-" gorm:"primaryKey"`
	UserID     uint      `json:"-" gorm:"uniqueIndex:ux_message_device,priority:1;not null"`
	DeviceID   string    `json:"device_id" gorm:"uniqueIndex:ux_message_device,priority:2;size:36;not null"`
	CreatedAt  time.Time `json:"created_at" gorm:"not null"`
	LastSeenAt time.Time `json:"last_seen_at" gorm:"index;not null"`
}

type SchemaMigration struct {
	Key       string `gorm:"primaryKey;size:80"`
	AppliedAt time.Time
}

type ChatGroup struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	OwnerID   uint      `json:"owner_id" gorm:"index;not null"`
	Name      string    `json:"name" gorm:"size:80;not null"`
	CreatedAt time.Time `json:"created_at"`
}

type GroupMember struct {
	ID       uint      `gorm:"primaryKey"`
	GroupID  uint      `gorm:"uniqueIndex:ux_group_member,priority:1;not null"`
	UserID   uint      `gorm:"uniqueIndex:ux_group_member,priority:2;index;not null"`
	JoinedAt time.Time `gorm:"not null"`
}

type GroupMessage struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	GroupID   uint      `json:"group_id" gorm:"index:idx_group_message,priority:1;not null"`
	SenderID  uint      `json:"sender_id" gorm:"uniqueIndex:ux_group_client,priority:1;not null"`
	ClientID  string    `json:"client_id" gorm:"uniqueIndex:ux_group_client,priority:2;size:80;not null"`
	Body      string    `json:"body" gorm:"size:4000;not null"`
	CreatedAt time.Time `json:"created_at" gorm:"index:idx_group_message,priority:2"`
}

type GroupRead struct {
	ID        uint `gorm:"primaryKey"`
	GroupID   uint `gorm:"uniqueIndex:ux_group_read,priority:1;not null"`
	UserID    uint `gorm:"uniqueIndex:ux_group_read,priority:2;not null"`
	LastAckID uint `gorm:"not null;default:0"`
}

// GroupDeviceRead advances independently of the account-level legacy cursor.
type GroupDeviceRead struct {
	ID        uint   `gorm:"primaryKey"`
	GroupID   uint   `gorm:"uniqueIndex:ux_group_device_read,priority:1;not null"`
	UserID    uint   `gorm:"uniqueIndex:ux_group_device_read,priority:2;index:idx_group_device_owner,priority:1;not null"`
	DeviceID  string `gorm:"uniqueIndex:ux_group_device_read,priority:3;index:idx_group_device_owner,priority:2;size:36;not null"`
	LastAckID uint   `gorm:"not null;default:0"`
}

type OutboxEvent struct {
	ID          uint      `gorm:"primaryKey"`
	EventKey    string    `gorm:"uniqueIndex;size:100;not null"`
	Topic       string    `gorm:"size:60;not null"`
	Payload     string    `gorm:"type:text;not null"`
	Attempts    int       `gorm:"not null;default:0"`
	AvailableAt time.Time `gorm:"index"`
	PublishedAt *time.Time
	DeadAt      *time.Time
	LastError   string `gorm:"size:500"`
	CreatedAt   time.Time
}
