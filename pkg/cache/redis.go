package cacheredis

import (
	"context"

	"encoding/json"

	"github.com/go-redis/redis/v8"
)

var ctx = context.Background()

var redisClient = redis.NewClient(&redis.Options{
	Addr:     "localhost:6379",
	Password: "",
	DB:       0,
})

func GetCacheRedis(key string) ([]string, error) {
	var result []string
	var err error
	ssiCache, errGet := redisClient.Get(ctx, key).Result()
	err = errGet
	if ssiCache != "" {
		errPaser := json.Unmarshal([]byte(ssiCache), &result)
		err = errPaser
	}
	return result, err
}
