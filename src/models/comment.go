package models

import (
	"GuGoTik/src/storage/database"
	"gorm.io/gorm"
)

type Comment struct {
	ID                uint32 `gorm:"not null;primaryKey;autoIncrement"`                              // 评论 ID
	VideoId           uint32 `json:"video_id" column:"video_id" gorm:"not null;index:comment_video"` // 视频 ID
	UserId            uint32 `json:"user_id" column:"user_id" gorm:"not null"`                       // 用户 ID
	Content           string `json:"content" column:"content"`                                       // 评论内容
	Rate              uint32 `gorm:"index:comment_video"`                                            // 记录评论的评分
	Reason            string // 存储评论的原因或理由
	ModerationFlagged bool   // 评论是否被标记为需要审核

	// 这些字段用于表示评论是否包含特定类型的内容，如仇恨言论、威胁性言论、自残内容、性内容、未成年人性内容、暴力内容等
	ModerationHate            bool
	ModerationHateThreatening bool
	ModerationSelfHarm        bool
	ModerationSexual          bool
	ModerationSexualMinors    bool
	ModerationViolence        bool
	ModerationViolenceGraphic bool
	gorm.Model
}

func init() {
	if err := database.Client.AutoMigrate(&Comment{}); err != nil {
		panic(err)
	}
}
