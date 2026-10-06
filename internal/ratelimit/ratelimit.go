package ratelimit

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// slidingWindowLua is an atomic Lua script that runs entirely inside Redis.
// Because Redis is single-threaded and Lua scripts run atomically, no two
// gateway replicas can interleave their check-and-increment steps, which
// is what guarantees the limit is global across all instances.
//
// Algorithm: sliding window log
//  1. Remove timestamps older than (now - window)
//  2. Count remaining entries
//  3. If count >= limit: reject
//  4. Else: record this request (unique member, so two requests in the same
//     millisecond are both counted); allow
const slidingWindowLua = `
local key    = KEYS[1]
local limit  = tonumber(ARGV[1])
local window = tonumber(ARGV[2])   -- seconds
local now    = tonumber(ARGV[3])   -- unix milliseconds
local member = ARGV[4]             -- unique per request
local floor  = now - (window * 1000)

redis.call("ZREMRANGEBYSCORE", key, 0, floor)
local count = redis.call("ZCARD", key)

if count >= limit then
    return 0
end

redis.call("ZADD", key, now, member)
redis.call("EXPIRE", key, window)
return 1
`

type Limiter struct {
	rdb    *redis.Client
	script *redis.Script
}

func New(rdb *redis.Client) *Limiter {
	return &Limiter{
		rdb:    rdb,
		script: redis.NewScript(slidingWindowLua),
	}
}

func (l *Limiter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		clientID := c.GetString("client_id")
		rateLimit := c.GetInt("rate_limit")
		windowSecs := c.GetInt("window_seconds")
		tier := c.GetString("client_tier")

		if rateLimit <= 0 {
			rateLimit = 100
		}
		if windowSecs <= 0 {
			windowSecs = 60
		}

		key := "ratelimit:" + clientID
		nowMs := time.Now().UnixMilli()

		result, err := l.script.Run(
			context.Background(),
			l.rdb,
			[]string{key},
			rateLimit,
			windowSecs,
			nowMs,
			fmt.Sprintf("%d-%d", nowMs, rand.Int63()),
		).Int()

		// Rate-limit headers — same pattern as GitHub/Stripe APIs
		c.Header("X-RateLimit-Limit", strconv.Itoa(rateLimit))
		c.Header("X-RateLimit-Tier", tier)

		if err != nil {
			// Redis is down: this is a dependency failure, not a client error.
			// Fail closed, but say so with 503 rather than a misleading 429.
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "rate limiter unavailable"})
			return
		}
		if result == 0 {
			c.Header("X-Gateway-Rejected", "rate-limit")
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "rate limit exceeded",
				"tier":  tier,
				"limit": rateLimit,
			})
			return
		}
		c.Next()
	}
}
