package models

import (
	"GuGoTik/src/rpc/feed"
)

type ListVideosReq struct {
	LatestTime string `form:"latest_time"` // 可选参数，限制返回视频的最新投稿时间戳，精确到秒，不填表示当前时间
	ActorId    int    `form:"actor_id"`
}

type ListVideosRes struct {
	StatusCode int           `json:"status_code"`
	StatusMsg  string        `json:"status_msg"`
	NextTime   *int64        `json:"next_time,omitempty"` // 本次返回的视频中，发布最早的时间，作为下次请求时的latest_time
	VideoList  []*feed.Video `json:"video_list,omitempty"`
}
