package main

import (
	"GuGoTik/src/constant/config"
	"GuGoTik/src/constant/strings"
	"GuGoTik/src/extra/tracing"
	"GuGoTik/src/models"
	"GuGoTik/src/rpc/favorite"
	"GuGoTik/src/rpc/feed"
	"GuGoTik/src/rpc/user"
	redis2 "GuGoTik/src/storage/redis"
	"GuGoTik/src/utils/audit"
	grpc2 "GuGoTik/src/utils/grpc"
	"GuGoTik/src/utils/logging"
	"GuGoTik/src/utils/rabbitmq"
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel/trace"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"
)

var feedClient feed.FeedServiceClient
var userClient user.UserServiceClient

var conn *amqp.Connection

var channel *amqp.Channel

type FavoriteServiceServerImpl struct {
	favorite.FavoriteServiceServer
}

func exitOnError(err error) {
	if err != nil {
		panic(err)
	}
}

func (c FavoriteServiceServerImpl) New() {
	feedRpcConn := grpc2.Connect(config.FeedRpcServerName)
	feedClient = feed.NewFeedServiceClient(feedRpcConn)
	userRpcConn := grpc2.Connect(config.UserRpcServerName)
	userClient = user.NewUserServiceClient(userRpcConn)

	var err error

	conn, err = amqp.Dial(rabbitmq.BuildMQConnAddr())
	exitOnError(err)

	channel, err = conn.Channel()
	exitOnError(err)

	err = channel.ExchangeDeclare(
		strings.EventExchange,
		"topic",
		true,
		false,
		false,
		false,
		nil,
	)
	exitOnError(err)
}

func CloseMQConn() {
	if err := conn.Close(); err != nil {
		panic(err)
	}

	if err := channel.Close(); err != nil {
		panic(err)
	}
}

func produceFavorite(ctx context.Context, event models.RecommendEvent) {
	ctx, span := tracing.Tracer.Start(ctx, "FavoriteEventPublisher")
	defer span.End()
	logging.SetSpanWithHostname(span)
	logger := logging.LogService("FavoriteService.FavoriteEventPublisher").WithContext(ctx)
	data, err := json.Marshal(event) // 序列化
	if err != nil {
		logger.WithFields(logrus.Fields{
			"err": err,
		}).Errorf("Error when marshal the event model")
		logging.SetSpanError(span, err)
		return
	}

	headers := rabbitmq.InjectAMQPHeaders(ctx) // 注入headers

	err = channel.PublishWithContext(ctx,
		strings.EventExchange,
		strings.FavoriteActionEvent,
		false,
		false,
		amqp.Publishing{
			ContentType: "text/plain",
			Body:        data,
			Headers:     headers,
		})

	if err != nil {
		logger.WithFields(logrus.Fields{
			"err": err,
		}).Errorf("Error when publishing the event model")
		logging.SetSpanError(span, err)
		return
	}
}

// 自身服务调用：用户点赞/取消点赞
func (c FavoriteServiceServerImpl) FavoriteAction(ctx context.Context, req *favorite.FavoriteRequest) (resp *favorite.FavoriteResponse, err error) {
	ctx, span := tracing.Tracer.Start(ctx, "FavoriteServiceServerImpl")
	defer span.End()
	logging.SetSpanWithHostname(span)
	logger := logging.LogService("FavoriteService.FavoriteAction").WithContext(ctx)

	logger.WithFields(logrus.Fields{
		"ActorId":     req.ActorId,
		"video_id":    req.VideoId,
		"action_type": req.ActionType, // 1=点赞 2=取消点赞
	}).Debugf("Process start")

	// 1. 检查video是否存在
	videoExistResp, err := feedClient.QueryVideoExisted(ctx, &feed.VideoExistRequest{
		VideoId: req.VideoId,
	})
	if err != nil {
		logger.WithFields(logrus.Fields{
			"err": err,
		}).Errorf("Query video existence happens error")
		logging.SetSpanError(span, err)
		resp = &favorite.FavoriteResponse{
			StatusCode: strings.FeedServiceInnerErrorCode,
			StatusMsg:  strings.FeedServiceInnerError,
		}
		return
	}
	// 若视频不存在
	if !videoExistResp.Existed {
		logger.WithFields(logrus.Fields{
			"VideoId": req.VideoId,
		}).Errorf("Video ID does not exist")
		logging.SetSpanError(span, err)
		resp = &favorite.FavoriteResponse{
			StatusCode: strings.UnableToQueryVideoErrorCode,
			StatusMsg:  strings.UnableToQueryVideoError,
		}
		return
	}
	// 2. 获取详细视频信息
	VideosRes, err := feedClient.QueryVideos(ctx, &feed.QueryVideosRequest{
		ActorId: req.ActorId, // 操作者id
		VideoIds: []uint32{
			req.VideoId, // 要查询的视频id
		},
	})

	if err != nil || VideosRes.StatusCode != strings.ServiceOKCode {
		logger.WithFields(logrus.Fields{
			"ActorId":     req.ActorId,
			"video_id":    req.VideoId,
			"action_type": req.ActionType, //点赞 1 2 取消点赞
		}).Errorf("FavoriteAction call feed Service error")
		logging.SetSpanError(span, err)

		return &favorite.FavoriteResponse{
			StatusCode: strings.FavoriteServiceErrorCode,
			StatusMsg:  strings.FavoriteServiceError,
		}, err
	}

	userLiked := VideosRes.VideoList[0].Author.Id // 被点赞用户的id
	// 使用zset保存点赞数据 key：userId -> {[]value：videoId, []score：点赞的时间戳}
	userId := fmt.Sprintf("%suser_like_%d", config.EnvCfg.RedisPrefix, req.ActorId)
	videoId := fmt.Sprintf("%d", req.VideoId)
	// 3. 检查当前点赞记录是否存在，防止重复点赞
	// 查询redis，当前记录存在，返回一个时间戳，当前记录不存在，返回redis.Nil
	value, err := redis2.Client.ZScore(ctx, userId, videoId).Result()

	// redis.Nil以外的错误
	if err != redis.Nil && err != nil {
		logger.WithFields(logrus.Fields{
			"ActorId":  req.ActorId,
			"video_id": req.VideoId,
			"err":      err,
		}).Errorf("redis Service error")
		logging.SetSpanError(span, err)

		return
	}
	// redis中不存在该点赞记录
	if err == redis.Nil {
		err = nil
	}
	// 4. 处理重复点赞 1=点赞 2=取消点赞
	if req.ActionType == 1 {
		// 时间戳存在 说明重复点赞
		if value > 0 {
			resp = &favorite.FavoriteResponse{
				StatusCode: strings.FavoriteServiceDuplicateCode,
				StatusMsg:  strings.FavoriteServiceDuplicateError,
			}
			logger.WithFields(logrus.Fields{
				"ActorId":  req.ActorId,
				"video_id": req.VideoId,
			}).Info("user duplicate like")
			return
		} else { // 正常点赞，开启redis事务
			_, err = redis2.Client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				videoId := fmt.Sprintf("%svideo_like_%d", config.EnvCfg.RedisPrefix, req.VideoId)   // 该视频的点赞数量，存入string
				userLikedId := fmt.Sprintf("%suser_liked_%d", config.EnvCfg.RedisPrefix, userLiked) // 被赞用户的获赞总量，存入string
				userLikeId := fmt.Sprintf("%suser_like_%d", config.EnvCfg.RedisPrefix, req.ActorId) // 用户的点赞记录，存入zset
				pipe.IncrBy(ctx, videoId, 1)
				pipe.IncrBy(ctx, userLikedId, 1)
				pipe.ZAdd(ctx, userLikeId, redis.Z{Score: float64(time.Now().Unix()), Member: req.VideoId})
				return nil
			})
			// Publish event to event_exchange and audit_exchange
			wg := sync.WaitGroup{}
			wg.Add(2)
			go func() {
				defer wg.Done()
				produceFavorite(ctx, models.RecommendEvent{
					ActorId: req.ActorId,
					VideoId: []uint32{req.VideoId},
					Type:    2,
					Source:  config.FavoriteRpcServerName,
				})
			}()
			go func() {
				defer wg.Done()
				action := &models.Action{
					Type:         strings.FavoriteIdActionLog,
					Name:         strings.FavoriteNameActionLog,
					SubName:      strings.FavoriteUpActionSubLog,
					ServiceName:  strings.FavoriteServiceName,
					ActorId:      req.ActorId,
					VideoId:      req.VideoId,
					AffectAction: 1,
					AffectedData: "1",
					EventId:      uuid.New().String(),
					TraceId:      trace.SpanContextFromContext(ctx).TraceID().String(),
					SpanId:       trace.SpanContextFromContext(ctx).SpanID().String(),
				}
				audit.PublishAuditEvent(ctx, action, channel)
			}()
			wg.Wait()
			if err == redis.Nil {
				err = nil
			}
		}
	} else {
		// 时间戳不存在 说明重复取消点赞
		if value == 0 {
			resp = &favorite.FavoriteResponse{
				StatusCode: strings.FavoriteServiceCancelCode,
				StatusMsg:  strings.FavoriteServiceCancelError,
			}

			logger.WithFields(logrus.Fields{
				"ActorId":  req.ActorId,
				"video_id": req.VideoId,
			}).Info("User did not like, cancel liking")
			return
		} else { // 正常取消点赞
			_, err = redis2.Client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				videoId := fmt.Sprintf("%svideo_like_%d", config.EnvCfg.RedisPrefix, req.VideoId)     // 该视频的点赞数量
				user_liked_id := fmt.Sprintf("%suser_liked_%d", config.EnvCfg.RedisPrefix, userLiked) // 被赞用户的获赞数量
				user_like_Id := fmt.Sprintf("%suser_like_%d", config.EnvCfg.RedisPrefix, req.ActorId) // 用户的点赞
				pipe.IncrBy(ctx, videoId, -1)
				pipe.IncrBy(ctx, user_liked_id, -1)
				pipe.ZRem(ctx, user_like_Id, req.VideoId)
				return nil
			})

			// Publish event to event_exchange and audit_exchange
			wg := sync.WaitGroup{}
			wg.Add(1)
			go func() {
				defer wg.Done()
				action := &models.Action{
					Type:         strings.FavoriteIdActionLog,
					Name:         strings.FavoriteNameActionLog,
					SubName:      strings.FavoriteDownActionSubLog,
					ServiceName:  strings.FavoriteServiceName,
					ActorId:      req.ActorId,
					VideoId:      req.VideoId,
					AffectAction: 1,
					AffectedData: "-1",
					EventId:      uuid.New().String(),
					TraceId:      trace.SpanContextFromContext(ctx).TraceID().String(),
					SpanId:       trace.SpanContextFromContext(ctx).SpanID().String(),
				}
				audit.PublishAuditEvent(ctx, action, channel)
			}()
			wg.Wait()

			if err == redis.Nil {
				err = nil
			}
		}

	}

	if err != nil {
		logger.WithFields(logrus.Fields{
			"ActorId":     req.ActorId,
			"video_id":    req.VideoId,
			"action_type": req.ActionType, //点赞 1 2 取消点赞
		}).Errorf("redis Service error")
		logging.SetSpanError(span, err)

		return &favorite.FavoriteResponse{
			StatusCode: strings.FavoriteServiceErrorCode,
			StatusMsg:  strings.FavoriteServiceError,
		}, err
	}
	resp = &favorite.FavoriteResponse{
		StatusCode: strings.ServiceOKCode,
		StatusMsg:  strings.ServiceOK,
	}
	logger.WithFields(logrus.Fields{
		"response": resp,
	}).Debugf("Process done.")
	return
}

// 自身服务调用：用户点赞的视频列表
func (c FavoriteServiceServerImpl) FavoriteList(ctx context.Context, req *favorite.FavoriteListRequest) (resp *favorite.FavoriteListResponse, err error) {

	ctx, span := tracing.Tracer.Start(ctx, "FavoriteServiceServerImpl")
	defer span.End()
	logging.SetSpanWithHostname(span)
	logger := logging.LogService("FavoriteService.FavoriteList").WithContext(ctx)

	logger.WithFields(logrus.Fields{
		"ActorId": req.ActorId,
		"user_id": req.UserId,
	}).Debugf("Process start")

	// 1. 判断用户id是否存在
	userResponse, err := userClient.GetUserExistInformation(ctx, &user.UserExistRequest{
		UserId: req.UserId,
	})

	if err != nil || userResponse.StatusCode != strings.ServiceOKCode {
		logger.WithFields(logrus.Fields{
			"err":     err,
			"ActorId": req.ActorId,
			"user_id": req.UserId,
		}).Errorf("User service error")
		logging.SetSpanError(span, err)

		return &favorite.FavoriteListResponse{
			StatusCode: strings.FavoriteServiceErrorCode,
			StatusMsg:  strings.FavoriteServiceError,
		}, err
	}

	if !userResponse.Existed {
		resp = &favorite.FavoriteListResponse{
			StatusCode: strings.UserDoNotExistedCode,
			StatusMsg:  strings.UserDoNotExisted,
		}
		return
	}
	// 2. 获取用户点赞的视频id列表：ZRevRange获取key在指定排名内的所有成员，0表示最高分的成员，-1表示最低分的成员
	userId := fmt.Sprintf("%suser_like_%d", config.EnvCfg.RedisPrefix, req.UserId)
	arr, err := redis2.Client.ZRevRange(ctx, userId, 0, -1).Result()
	if err != nil {
		logger.WithFields(logrus.Fields{
			"ActorId": req.ActorId,
			"user_id": req.UserId,
		}).Errorf("redis Service error")
		logging.SetSpanError(span, err)

		return &favorite.FavoriteListResponse{
			StatusCode: strings.FavoriteServiceErrorCode,
			StatusMsg:  strings.FavoriteServiceError,
		}, err
	}
	if len(arr) == 0 {
		resp = &favorite.FavoriteListResponse{
			StatusCode: strings.ServiceOKCode,
			StatusMsg:  strings.ServiceOK,
			VideoList:  nil,
		}
		return resp, nil
	}

	// 将string数组转换为uint32数组
	res := make([]uint32, len(arr))
	for index, val := range arr {
		num, _ := strconv.Atoi(val)
		res[index] = uint32(num)
	}
	// 3. 调用feed服务查询具体视频信息
	var VideoList []*feed.Video
	value, err := feedClient.QueryVideos(ctx, &feed.QueryVideosRequest{
		ActorId:  req.ActorId,
		VideoIds: res,
	})
	if err != nil || value.StatusCode != strings.ServiceOKCode {
		logger.WithFields(logrus.Fields{
			"ActorId": req.ActorId,
			"user_id": req.UserId,
		}).Errorf("feed Service error")
		logging.SetSpanError(span, err)
		return &favorite.FavoriteListResponse{
			StatusCode: strings.FavoriteServiceErrorCode,
			StatusMsg:  strings.FavoriteServiceError,
		}, err
	}

	VideoList = value.VideoList

	resp = &favorite.FavoriteListResponse{
		StatusCode: strings.ServiceOKCode,
		StatusMsg:  strings.ServiceOK,
		VideoList:  VideoList,
		// VideoList: nil,
	}
	return resp, nil
}

// 其他服务调用：用户是否喜欢该视频
func (c FavoriteServiceServerImpl) IsFavorite(ctx context.Context, req *favorite.IsFavoriteRequest) (resp *favorite.IsFavoriteResponse, err error) {
	ctx, span := tracing.Tracer.Start(ctx, "FavoriteServiceServerImpl")
	defer span.End()
	logging.SetSpanWithHostname(span)
	logger := logging.LogService("FavoriteService.IsFavorite").WithContext(ctx)

	logger.WithFields(logrus.Fields{
		"ActorId":  req.ActorId,
		"video_id": req.VideoId,
	}).Debugf("Process start")
	// 判断视频id是否存在
	value, err := feedClient.QueryVideoExisted(ctx, &feed.VideoExistRequest{
		VideoId: req.VideoId,
	})
	if err != nil || value.StatusCode != strings.ServiceOKCode {
		logger.WithFields(logrus.Fields{
			"ActorId": req.ActorId,
			"user_id": req.VideoId,
		}).Errorf("feed Service error")
		logging.SetSpanError(span, err)
		return &favorite.IsFavoriteResponse{
			StatusCode: strings.FavoriteServiceErrorCode,
			StatusMsg:  strings.FavoriteServiceError,
		}, err
	}

	userId := fmt.Sprintf("%suser_like_%d", config.EnvCfg.RedisPrefix, req.ActorId)
	videoId := fmt.Sprintf("%d", req.VideoId)

	// 查询redis，当前记录存在，返回一个时间戳，当前记录不存在，返回redis.Nil
	ok, err := redis2.Client.ZScore(ctx, userId, videoId).Result()

	if err == redis.Nil {
		err = nil
	} else if err != nil {
		logger.WithFields(logrus.Fields{
			"ActorId":  req.ActorId,
			"video_id": req.VideoId,
		}).Errorf("redis Service error")
		logging.SetSpanError(span, err)

		return &favorite.IsFavoriteResponse{
			StatusCode: strings.FavoriteServiceErrorCode,
			StatusMsg:  strings.FavoriteServiceError,
		}, err
	}

	if ok != 0 {
		resp = &favorite.IsFavoriteResponse{
			StatusCode: strings.ServiceOKCode,
			StatusMsg:  strings.ServiceOK,
			Result:     true,
		}
	} else {
		resp = &favorite.IsFavoriteResponse{
			StatusCode: strings.ServiceOKCode,
			StatusMsg:  strings.ServiceOK,
			Result:     false,
		}
	}
	logger.WithFields(logrus.Fields{
		"response": resp,
	}).Debugf("Process done.")
	return

}

// 其他服务调用：获取该视频的点赞数量
func (c FavoriteServiceServerImpl) CountFavorite(ctx context.Context, req *favorite.CountFavoriteRequest) (resp *favorite.CountFavoriteResponse, err error) {

	ctx, span := tracing.Tracer.Start(ctx, "FavoriteServiceServerImpl")
	defer span.End()
	logging.SetSpanWithHostname(span)
	logger := logging.LogService("FavoriteService.CountFavorite").WithContext(ctx)

	logger.WithFields(logrus.Fields{
		"video_id": req.VideoId,
	}).Debugf("Process start")
	// 判断视频id是否存在
	Vresp, err := feedClient.QueryVideoExisted(ctx, &feed.VideoExistRequest{
		VideoId: req.VideoId,
	})
	if err != nil || Vresp.StatusCode != strings.ServiceOKCode {
		logger.WithFields(logrus.Fields{
			"user_id": req.VideoId,
		}).Errorf("feed Service error")
		logging.SetSpanError(span, err)
		return &favorite.CountFavoriteResponse{
			StatusCode: strings.FavoriteServiceErrorCode,
			StatusMsg:  strings.FavoriteServiceError,
		}, err
	}
	// 获取该视频的点赞数量
	videoId := fmt.Sprintf("%svideo_like_%d", config.EnvCfg.RedisPrefix, req.VideoId)
	value, err := redis2.Client.Get(ctx, videoId).Result()
	var num int
	if err == redis.Nil {
		num = 0
		err = nil
	} else if err != nil {
		logger.WithFields(logrus.Fields{
			"video_id": req.VideoId,
		}).Errorf("redis Service error")
		logging.SetSpanError(span, err)

		return &favorite.CountFavoriteResponse{
			StatusCode: strings.FavoriteServiceErrorCode,
			StatusMsg:  strings.FavoriteServiceError,
		}, err
	} else {
		num, _ = strconv.Atoi(value)
	}
	resp = &favorite.CountFavoriteResponse{
		StatusCode: strings.ServiceOKCode,
		StatusMsg:  strings.ServiceOK,
		Count:      uint32(num),
	}
	logger.WithFields(logrus.Fields{
		"response": resp,
	}).Debugf("Process done.")
	return
}

// 其他服务调用：获取用户点赞的视频数量
func (c FavoriteServiceServerImpl) CountUserFavorite(ctx context.Context, req *favorite.CountUserFavoriteRequest) (resp *favorite.CountUserFavoriteResponse, err error) {

	ctx, span := tracing.Tracer.Start(ctx, "FavoriteServiceServerImpl")
	defer span.End()
	logging.SetSpanWithHostname(span)
	logger := logging.LogService("FavoriteService.CountUserFavorite").WithContext(ctx)

	logger.WithFields(logrus.Fields{
		"user_id": req.UserId,
	}).Debugf("Process start")

	// 判断用户id是否存在
	userResponse, err := userClient.GetUserExistInformation(ctx, &user.UserExistRequest{
		UserId: req.UserId,
	})

	if err != nil || userResponse.StatusCode != strings.ServiceOKCode {
		logger.WithFields(logrus.Fields{
			"err":     err,
			"ActorId": req.UserId,
		}).Errorf("User service error")
		logging.SetSpanError(span, err)

		return &favorite.CountUserFavoriteResponse{
			StatusCode: strings.FavoriteServiceErrorCode,
			StatusMsg:  strings.FavoriteServiceError,
		}, err
	}

	if !userResponse.Existed {
		resp = &favorite.CountUserFavoriteResponse{
			StatusCode: strings.UserDoNotExistedCode,
			StatusMsg:  strings.UserDoNotExisted,
		}
		return
	}
	// ZCard用于获取zset中成员的数量，即用户点赞的视频数量
	userLikeId := fmt.Sprintf("%suser_like_%d", config.EnvCfg.RedisPrefix, req.UserId)
	value, err := redis2.Client.ZCard(ctx, userLikeId).Result()
	var num int64
	if err == redis.Nil {
		num = 0
		err = nil
	} else if err != nil {
		logger.WithFields(logrus.Fields{
			"user_id": req.UserId,
		}).Errorf("redis Service error")
		logging.SetSpanError(span, err)

		return &favorite.CountUserFavoriteResponse{
			StatusCode: strings.FavoriteServiceErrorCode,
			StatusMsg:  strings.FavoriteServiceError,
		}, err
	} else {
		num = value
	}

	resp = &favorite.CountUserFavoriteResponse{
		StatusCode: strings.ServiceOKCode,
		StatusMsg:  strings.ServiceOK,
		Count:      uint32(num),
	}
	logger.WithFields(logrus.Fields{
		"response": resp,
	}).Debugf("Process done.")
	return
}

// 其他服务调用：获取用户总共获得的点赞数量
func (c FavoriteServiceServerImpl) CountUserTotalFavorited(ctx context.Context, req *favorite.CountUserTotalFavoritedRequest) (resp *favorite.CountUserTotalFavoritedResponse, err error) {
	ctx, span := tracing.Tracer.Start(ctx, "FavoriteServiceServerImpl")
	defer span.End()
	logging.SetSpanWithHostname(span)
	logger := logging.LogService("FavoriteService.CountUserTotalFavorited").WithContext(ctx)

	logger.WithFields(logrus.Fields{
		"ActorId": req.ActorId,
		"user_id": req.UserId,
	}).Debugf("Process start")

	// 判断用户id是否存在
	userResponse, err := userClient.GetUserExistInformation(ctx, &user.UserExistRequest{
		UserId: req.UserId,
	})

	if err != nil || userResponse.StatusCode != strings.ServiceOKCode {
		logger.WithFields(logrus.Fields{
			"err":     err,
			"ActorId": req.UserId,
			"user_id": req.UserId,
		}).Errorf("User service error")
		logging.SetSpanError(span, err)

		return &favorite.CountUserTotalFavoritedResponse{
			StatusCode: strings.FavoriteServiceErrorCode,
			StatusMsg:  strings.FavoriteServiceError,
		}, err
	}

	if !userResponse.Existed {
		resp = &favorite.CountUserTotalFavoritedResponse{
			StatusCode: strings.UserDoNotExistedCode,
			StatusMsg:  strings.UserDoNotExisted,
		}
		return
	}

	// 获取用户总共获得的点赞数量
	userLikedId := fmt.Sprintf("%suser_liked_%d", config.EnvCfg.RedisPrefix, req.UserId)
	value, err := redis2.Client.Get(ctx, userLikedId).Result()
	var num int
	if err == redis.Nil {
		num = 0
		err = nil
	} else if err != nil {
		logger.WithFields(logrus.Fields{
			"err":     err,
			"user_id": req.UserId,
			"ActorId": req.ActorId,
		}).Errorf("redis Service error")
		logging.SetSpanError(span, err)

		return &favorite.CountUserTotalFavoritedResponse{
			StatusCode: strings.FavoriteServiceErrorCode,
			StatusMsg:  strings.FavoriteServiceError,
		}, err
	} else {
		num, _ = strconv.Atoi(value)
	}
	resp = &favorite.CountUserTotalFavoritedResponse{
		StatusCode: strings.ServiceOKCode,
		StatusMsg:  strings.ServiceOK,
		Count:      uint32(num),
	}
	logger.WithFields(logrus.Fields{
		"response": resp,
	}).Debugf("Process done.")
	return
}
