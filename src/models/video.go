package models

import (
	"GuGoTik/src/storage/database"
	"gorm.io/gorm"
)

// Video 视频表
type Video struct {
	ID            uint32 `gorm:"not null;primaryKey;"`
	UserId        uint32 `json:"user_id" gorm:"not null;"`
	Title         string `json:"title" gorm:"not null;"`
	FileName      string `json:"play_name" gorm:"not null;"`  // 存储视频文件的名称
	CoverName     string `json:"cover_name" gorm:"not null;"` // 存储视频封面图片的名称
	AudioFileName string // 存储音频文件的名称
	Transcript    string // 存储视频的文本转录内容
	Summary       string // 存储视频的摘要信息
	Keywords      string // e.g., "keywords1 | keywords2 | keywords3"
	gorm.Model
}

func init() {
	if err := database.Client.AutoMigrate(&Video{}); err != nil {
		panic(err)
	}
}
