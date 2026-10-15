package platform

import "time"

type User struct {
	ID           uint      `json:"id" gorm:"primaryKey"`
	Username     string    `json:"username" gorm:"size:32;uniqueIndex;not null"`
	PasswordHash string    `json:"-" gorm:"not null"`
	Nickname     string    `json:"nickname" gorm:"size:48;not null"`
	Bio          string    `json:"bio" gorm:"size:280"`
	AvatarURL    string    `json:"avatar_url" gorm:"size:512"`
	CreatedAt    time.Time `json:"created_at"`
}

type Video struct {
	ID            uint      `json:"id" gorm:"primaryKey"`
	AuthorID      uint      `json:"author_id" gorm:"index:idx_video_author,priority:1;not null"`
	Author        User      `json:"author,omitempty" gorm:"foreignKey:AuthorID"`
	Title         string    `json:"title" gorm:"size:120;not null"`
	Description   string    `json:"description" gorm:"size:2000"`
	MediaURL      string    `json:"media_url" gorm:"size:512;not null"`
	CoverURL      string    `json:"cover_url" gorm:"size:512"`
	Status        string    `json:"status" gorm:"size:16;index:idx_video_feed,priority:1;not null;default:published"`
	LikeCount     int64     `json:"like_count"`
	CommentCount  int64     `json:"comment_count"`
	FavoriteCount int64     `json:"favorite_count"`
	PlayCount     int64     `json:"play_count"`
	CreatedAt     time.Time `json:"created_at" gorm:"index:idx_video_feed,priority:2,sort:desc;index:idx_video_author,priority:2,sort:desc"`
}

type Follow struct {
	ID         uint      `gorm:"primaryKey"`
	FollowerID uint      `gorm:"uniqueIndex:ux_follow_pair,priority:1;index:idx_followers,priority:2;not null"`
	FolloweeID uint      `gorm:"uniqueIndex:ux_follow_pair,priority:2;index:idx_followers,priority:1;not null"`
	CreatedAt  time.Time `gorm:"index:idx_following,priority:2,sort:desc"`
}

type VideoAction struct {
	ID        uint   `gorm:"primaryKey"`
	UserID    uint   `gorm:"uniqueIndex:ux_action,priority:1;not null"`
	VideoID   uint   `gorm:"uniqueIndex:ux_action,priority:2;index;not null"`
	Kind      string `gorm:"uniqueIndex:ux_action,priority:3;size:16;not null"`
	CreatedAt time.Time
}

type Comment struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	VideoID   uint      `json:"video_id" gorm:"index:idx_comment_page,priority:1;not null"`
	UserID    uint      `json:"user_id" gorm:"not null"`
	User      User      `json:"user,omitempty" gorm:"foreignKey:UserID"`
	ParentID  *uint     `json:"parent_id" gorm:"index"`
	Body      string    `json:"body" gorm:"size:2000;not null"`
	CreatedAt time.Time `json:"created_at" gorm:"index:idx_comment_page,priority:2,sort:desc"`
}

type Notification struct {
	ID        uint       `json:"id" gorm:"primaryKey"`
	UserID    uint       `json:"-" gorm:"uniqueIndex:ux_notification_event,priority:1;index:idx_notification_page,priority:1;not null"`
	EventKey  string     `json:"-" gorm:"uniqueIndex:ux_notification_event,priority:2;size:100;not null"`
	Kind      string     `json:"kind" gorm:"size:20;not null"`
	ActorID   uint       `json:"actor_id"`
	Actor     User       `json:"actor,omitempty" gorm:"foreignKey:ActorID"`
	VideoID   uint       `json:"video_id,omitempty"`
	Text      string     `json:"text" gorm:"size:280"`
	ReadAt    *time.Time `json:"read_at"`
	CreatedAt time.Time  `json:"created_at" gorm:"index:idx_notification_page,priority:2,sort:desc"`
}

type ChatMessage struct {
	ID          uint       `json:"id" gorm:"primaryKey"`
	ClientID    string     `json:"client_id" gorm:"uniqueIndex:ux_message_client,priority:1;size:80;not null"`
	SenderID    uint       `json:"sender_id" gorm:"uniqueIndex:ux_message_client,priority:2;index:idx_message_thread,priority:1;not null"`
	RecipientID uint       `json:"recipient_id" gorm:"index:idx_message_thread,priority:2;not null"`
	Body        string     `json:"body" gorm:"size:4000;not null"`
	CreatedAt   time.Time  `json:"created_at" gorm:"index:idx_message_thread,priority:3"`
	AckedAt     *time.Time `json:"acked_at"`
}

type OutboxEvent struct {
	ID          uint      `gorm:"primaryKey"`
	EventKey    string    `gorm:"uniqueIndex;size:100;not null"`
	Topic       string    `gorm:"size:60;not null"`
	Payload     string    `gorm:"type:text;not null"`
	Attempts    int       `gorm:"not null;default:0"`
	AvailableAt time.Time `gorm:"index"`
	PublishedAt *time.Time
	CreatedAt   time.Time
}
