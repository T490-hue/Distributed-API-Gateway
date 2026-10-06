package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"

	"github.com/T490-hue/Distributed-API-Gateway/internal/auth"
	"github.com/T490-hue/Distributed-API-Gateway/internal/logging"
	"github.com/T490-hue/Distributed-API-Gateway/internal/metrics"
	"github.com/T490-hue/Distributed-API-Gateway/internal/proxy"
	"github.com/T490-hue/Distributed-API-Gateway/internal/ratelimit"
)

func main() {
	// --- dependencies ---
	db, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(30 * time.Minute)

	rdb := newRedis()

	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		log.Fatal("JWT_SECRET must be set")
	}
	backendURL := os.Getenv("BACKEND_URL")
	logMode := os.Getenv("LOG_MODE") // "sync" | "async"  (default async)

	// --- logger: the benchmark toggle ---
	var logger logging.Logger
	var asyncLogger *logging.AsyncLogger
	if logMode == "sync" {
		logger = logging.NewSync(db)
	} else {
		logMode = "async"
		asyncLogger = logging.NewAsync(db, 20000)
		asyncLogger.Start()
		logger = asyncLogger
	}

	prom := metrics.New()

	// --- gin router ---
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.GET("/metrics", prom.Handler())

	// public auth routes
	authHandler := auth.NewHandler(db, jwtSecret, os.Getenv("ALLOW_SELF_SERVE_PREMIUM") == "true")
	r.POST("/v1/auth/signup", authHandler.Signup)
	r.POST("/v1/auth/login", authHandler.Login)

	// protected proxy routes.
	// Order matters: metrics and logging wrap the rate limiter so that rejected
	// (429) requests are still counted and logged. If the limiter ran first, it
	// would abort the chain and rejections would never be recorded.
	rl := ratelimit.New(rdb)
	prx := proxy.New(backendURL)

	v1 := r.Group("/v1/proxy")
	v1.Use(auth.Middleware(db, jwtSecret))
	v1.Use(prom.RequestMiddleware())
	v1.Use(logging.Middleware(logger))
	v1.Use(rl.Middleware())
	v1.Any("/*path", prx.Handle)

	// profile / logs routes
	r.GET("/v1/me", auth.Middleware(db, jwtSecret), authHandler.Me)
	r.GET("/v1/me/logs", auth.Middleware(db, jwtSecret), logging.LogsHandler(db))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown: stop accepting requests, then flush the async log queue.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("gateway starting on :%s  log_mode=%s", port, logMode)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	if asyncLogger != nil {
		asyncLogger.Stop() // drains buffered entries to Postgres
	}
}

// newRedis supports REDIS_URL (redis:// or rediss://, used by hosted Redis)
// and falls back to REDIS_ADDR (host:port, used by docker-compose).
func newRedis() *redis.Client {
	if u := os.Getenv("REDIS_URL"); u != "" {
		opt, err := redis.ParseURL(u)
		if err != nil {
			log.Fatalf("REDIS_URL: %v", err)
		}
		opt.PoolSize = 50
		return redis.NewClient(opt)
	}
	return redis.NewClient(&redis.Options{
		Addr:         os.Getenv("REDIS_ADDR"),
		PoolSize:     50,
		MinIdleConns: 10,
	})
}
