package models

import "GuGoTik/src/rpc/comment"

type ActionCommentReq struct {
	Token       string `form:"token" binding:"required"`
	ActorId     int    `form:"actor_id"`
	VideoId     int    `form:"video_id" binding:"-"`
	ActionType  int    `form:"action_type" binding:"required"` // 1-发布评论，2-删除评论
	CommentText string `form:"comment_text"`                   // 用户填写的评论内容，在action_type=1的时候使用
	CommentId   int    `form:"comment_id"`                     // 要删除的评论id，在action_type=2的时候使用
}

type ActionCommentRes struct {
	StatusCode int             `json:"status_code"`
	StatusMsg  string          `json:"status_msg"`
	Comment    comment.Comment `json:"comment"`
}

type ListCommentReq struct {
	Token   string `form:"token"`
	ActorId int    `form:"actor_id"`
	VideoId int    `form:"video_id" binding:"-"`
}

type ListCommentRes struct {
	StatusCode  int                `json:"status_code"`
	StatusMsg   string             `json:"status_msg"`
	CommentList []*comment.Comment `json:"comment_list"`
}

type CountCommentReq struct {
	Token   string `form:"token"`
	ActorId int    `form:"actor_id"`
	VideoId int    `form:"video_id" binding:"-"`
}

type CountCommentRes struct {
	StatusCode   int    `json:"status_code"`
	StatusMsg    string `json:"status_msg"`
	CommentCount int    `json:"comment_count"`
}
