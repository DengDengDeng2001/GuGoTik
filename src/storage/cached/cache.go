package cached

import (
	"GuGoTik/src/constant/config"
	"GuGoTik/src/extra/tracing"
	"GuGoTik/src/storage/database"
	"GuGoTik/src/storage/redis"
	"GuGoTik/src/utils/logging"
	"context"
	"github.com/patrickmn/go-cache"
	redis2 "github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"
	"math/rand"
	"reflect"
	"strconv"
	"sync"
	"time"
)

// 表示 Redis 随机缓存的时间范围
const redisRandomScope = 1

// 不同数据存储到多个类型cache，例如stringsCache, userinfoCache
var cacheMaps = make(map[string]*cache.Cache)

var m = new(sync.Mutex) // 互斥锁

// MultiCachedItem 用于转换结构体类型
type MultiCachedItem interface {
	GetID() uint32 // 获得id用于查询key
	IsDirty() bool // 是否查询到数据
}

// ScanGet 采用 Memory-Redis-DB 的模式读取结构体数据，并且填充到传入的结构体中
// 结构体需要实现 MultiCachedItem 接口且确保ID可用
// 参数obj无法直接调用实现的GetID()方法，需要这样调用：obj.(MultiCachedItem).GetID()
func ScanGet(ctx context.Context, key string, obj interface{}) (bool, error) {
	ctx, span := tracing.Tracer.Start(ctx, "Cached-GetFromScanCache")
	defer span.End()
	logging.SetSpanWithHostname(span)
	logger := logging.LogService("Cached.GetFromScanCache").WithContext(ctx)

	// 1. 在本地缓存查询数据
	key = config.EnvCfg.RedisPrefix + key
	c := getOrCreateCache(key)
	// 这里不使用 reflect.ValueOf(obj).Elem().FieldByName(xxx).Interface() 获取结构体内字段值
	// 是因为代码可读性较差，所以实现一个接口来获取指定结构体内字段值
	key = key + strconv.FormatUint(uint64(obj.(MultiCachedItem).GetID()), 10)
	if cachedData, found := c.Get(key); found {
		objValue := reflect.ValueOf(obj).Elem()   // 获取结构体指针指向的值
		objValue.Set(reflect.ValueOf(cachedData)) // 将缓存数据的值填充到结构体中
		return true, nil
	}
	logger.WithFields(logrus.Fields{
		"key": key,
	}).Infof("Missed local memory cached")

	// 2. 缓存没有命中，Fallback 到 Redis
	if err := redis.Client.HGetAll(ctx, key).Scan(obj); err != nil {
		if err != redis2.Nil {
			logger.WithFields(logrus.Fields{
				"err": err,
				"key": key,
			}).Errorf("Redis error when find struct")
			logging.SetSpanError(span, err)
			return false, err
		}
	}
	// Redis 存在数据，回写本地缓存
	if obj.(MultiCachedItem).IsDirty() {
		logger.WithFields(logrus.Fields{
			"key": key,
		}).Infof("Redis hit the key")
		c.Set(key, reflect.ValueOf(obj).Elem(), cache.DefaultExpiration)
		return true, nil
	}
	logger.WithFields(logrus.Fields{
		"key": key,
	}).Warnf("Missed Redis Cached")

	// 3. Redis 没有命中，Fallback 到 DB
	result := database.Client.WithContext(ctx).Find(obj)
	if result.RowsAffected == 0 {
		logger.WithFields(logrus.Fields{
			"key": key,
		}).Warnf("Missed DB obj, seems wrong key")
		return false, result.Error
	}
	// DB 存在数据，将数据回写 Redis
	if result := redis.Client.HSet(ctx, key, obj); result.Err() != nil {
		logger.WithFields(logrus.Fields{
			"err": result.Err(),
			"key": key,
		}).Errorf("Redis error when set struct info")
		logging.SetSpanError(span, result.Err())
		return false, nil
	}
	// 同时将数据回写 Local Memory
	c.Set(key, reflect.ValueOf(obj).Elem(), cache.DefaultExpiration)
	return true, nil
}

// CacheAndRedisGet 采用 Memory-Redis 的模式读取结构体类型
func CacheAndRedisGet(ctx context.Context, key string, obj interface{}) (bool, error) {
	ctx, span := tracing.Tracer.Start(ctx, "CacheAndRedisGet")
	defer span.End()
	logging.SetSpanWithHostname(span)
	logger := logging.LogService("CacheAndRedisGet").WithContext(ctx)

	// 1. 在本地缓存查询数据
	key = config.EnvCfg.RedisPrefix + key
	c := getOrCreateCache(key)
	// 这里不使用 reflect.ValueOf(obj).Elem().FieldByName(xxx).Interface() 获取结构体内字段值
	// 是因为代码可读性较差，所以实现一个接口来获取指定结构体内字段值
	key = key + strconv.FormatUint(uint64(obj.(MultiCachedItem).GetID()), 10)
	if cachedData, found := c.Get(key); found {
		objValue := reflect.ValueOf(obj).Elem()   // 获取结构体指针指向的值
		objValue.Set(reflect.ValueOf(cachedData)) // 将缓存数据的值填充到结构体中
		return true, nil
	}
	logger.WithFields(logrus.Fields{
		"key": key,
	}).Infof("Missed local memory cached")

	// 2. 缓存没有命中，Fallback 到 Redis
	if err := redis.Client.HGetAll(ctx, key).Scan(obj); err != nil {
		if err != redis2.Nil {
			logger.WithFields(logrus.Fields{
				"err": err,
				"key": key,
			}).Errorf("Redis error when find struct")
			logging.SetSpanError(span, err)
			return false, err
		}
	}
	// Redis 存在数据，回写本地缓存
	if obj.(MultiCachedItem).IsDirty() {
		logger.WithFields(logrus.Fields{
			"key": key,
		}).Infof("Redis hit the key")
		c.Set(key, reflect.ValueOf(obj).Elem(), cache.DefaultExpiration)
		return true, nil
	}
	logger.WithFields(logrus.Fields{
		"key": key,
	}).Warnf("Missed Redis Cached")
	return false, nil
}

// ScanWriteCache 写入本地缓存并获取Redis数据，如果 state 为 false 那么只会写入本地缓存
func ScanWriteCache(ctx context.Context, key string, obj interface{}, state bool) (err error) {
	ctx, span := tracing.Tracer.Start(ctx, "Cached-ScanWriteCache")
	defer span.End()
	logging.SetSpanWithHostname(span)
	logger := logging.LogService("Cached.ScanWriteCache").WithContext(ctx)
	key = config.EnvCfg.RedisPrefix + key

	wrappedObj := obj.(MultiCachedItem)
	key = key + strconv.FormatUint(uint64(wrappedObj.GetID()), 10)
	c := getOrCreateCache(key)
	c.Set(key, reflect.ValueOf(obj).Elem(), cache.DefaultExpiration) // 写入本地缓存

	if state {
		if err = redis.Client.HGetAll(ctx, key).Scan(obj); err != nil {
			logger.WithFields(logrus.Fields{
				"err": err,
				"key": key,
			}).Errorf("Redis error when find struct info")
			logging.SetSpanError(span, err)
			return err
		}
	}
	return err
}

// ScanTagDelete 删除 Memory-Redis 缓存，下次读取时会 FallBack 到 DB
func ScanTagDelete(ctx context.Context, key string, obj interface{}) {
	ctx, span := tracing.Tracer.Start(ctx, "Cached-ScanTagDelete")
	defer span.End()
	logging.SetSpanWithHostname(span)
	key = config.EnvCfg.RedisPrefix + key

	redis.Client.HDel(ctx, key) // 删除 Redis

	c := getOrCreateCache(key)
	wrappedObj := obj.(MultiCachedItem)
	key = key + strconv.FormatUint(uint64(wrappedObj.GetID()), 10)
	c.Delete(key) // 删除本地缓存
}

// Get 读取字符串缓存，先本地后 redis，其中找到了返回 True，没找到返回 False，异常也返回 False
func Get(ctx context.Context, key string) (string, bool, error) {
	ctx, span := tracing.Tracer.Start(ctx, "Cached-GetFromStringCache")
	defer span.End()
	logging.SetSpanWithHostname(span)
	logger := logging.LogService("Cached.GetFromStringCache").WithContext(ctx)
	key = config.EnvCfg.RedisPrefix + key

	c := getOrCreateCache("strings")
	if cachedData, found := c.Get(key); found {
		return cachedData.(string), true, nil
	}
	logger.WithFields(logrus.Fields{
		"key": key,
	}).Infof("Missed local memory cached")

	// 本地缓存没有命中，Fallback 到 Redis
	var result *redis2.StringCmd
	if result = redis.Client.Get(ctx, key); result.Err() != nil && result.Err() != redis2.Nil {
		logger.WithFields(logrus.Fields{
			"err":    result.Err(),
			"string": key,
		}).Errorf("Redis error when find string")
		logging.SetSpanError(span, result.Err())
		return "", false, nil
	}

	value, err := result.Result()

	switch {
	case err == redis2.Nil:
		return "", false, nil
	case err != nil:
		logger.WithFields(logrus.Fields{
			"err": err,
		}).Errorf("Err when write Redis")
		logging.SetSpanError(span, err)
		return "", false, err
	default:
		c.Set(key, value, cache.DefaultExpiration)
		return value, true, nil
	}
}

// GetWithFunc 封装了 Get，从本地缓存-Redis中获取字符串，如果不存在调用 Func 函数获取
func GetWithFunc(ctx context.Context, key string, f func(ctx context.Context, key string) (string, error)) (string, error) {
	ctx, span := tracing.Tracer.Start(ctx, "Cached-GetFromStringCacheWithFunc")
	defer span.End()
	logging.SetSpanWithHostname(span)
	value, ok, err := Get(ctx, key)

	if err != nil {
		return "", err
	}

	if ok {
		return value, nil
	}

	// 如果不存在，那么从func获取它
	value, err = f(ctx, key)

	if err != nil {
		return "", err
	}

	Write(ctx, key, value, true)
	return value, nil
}

// Write 写入字符串缓存，如果 state 为 false 则只写入 Local Memory，不写入 Redis
func Write(ctx context.Context, key string, value string, state bool) {
	ctx, span := tracing.Tracer.Start(ctx, "Cached-SetStringCache")
	defer span.End()
	logging.SetSpanWithHostname(span)
	key = config.EnvCfg.RedisPrefix + key

	c := getOrCreateCache("strings")
	c.Set(key, value, cache.DefaultExpiration)

	if state {
		redis.Client.Set(ctx, key, value, 120*time.Hour+time.Duration(rand.Intn(redisRandomScope))*time.Second)
	}
}

// TagDelete 删除字符串缓存
func TagDelete(ctx context.Context, key string) {
	ctx, span := tracing.Tracer.Start(ctx, "Cached-DeleteStringCache")
	defer span.End()
	logging.SetSpanWithHostname(span)
	key = config.EnvCfg.RedisPrefix + key

	c := getOrCreateCache("strings")
	c.Delete(key) // 删除本地缓存

	redis.Client.Del(ctx, key) // 删除Redis
}

// 获取或创建缓存对象
func getOrCreateCache(name string) *cache.Cache {
	// 单例模式 双重检查
	cc, ok := cacheMaps[name] // 根据指定的 name 获取缓存对象 cc
	if !ok {
		// 如果缓存对象不存在，进入加锁操作，避免并发创建相同名称的缓存对象
		m.Lock()
		defer m.Unlock()
		cc, ok := cacheMaps[name]
		if !ok {
			// 在加锁范围内，再次检查是否存在指定名称的缓存对象，如果不存在，则创建一个新的缓存对象 cc
			cc = cache.New(5*time.Minute, 10*time.Minute)
			cacheMaps[name] = cc
			return cc
		}
		// 存在则直接返回
		return cc
	}
	return cc
}

func ActionRedisSync(time time.Duration, f func(client redis2.UniversalClient) error) {
	go func() {
		daemon := NewTick(time, f)
		daemon.Start()
	}()
}
