// Package logging provides two Logger implementations behind one interface.
//
// SyncLogger writes every request log to Postgres before returning the
// response to the client. The database round-trip sits on the hot path,
// adding its latency to every proxied request.
//
// AsyncLogger drops the log entry into a buffered channel and returns
// immediately. A background goroutine drains the channel and writes batches to
// Postgres with COPY at its own pace. The client never waits on the database write.
//
// Toggle with LOG_MODE=sync (default: async).
// Run scripts/benchmark-logging.sh to see the p95 difference.
package logging

import (
	"database/sql"
	"log"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
)

// Logger is the interface both implementations satisfy.
type Logger interface {
	Log(entry Entry)
}

// Entry is one request log row.
type Entry struct {
	ClientID   string
	Path       string
	Method     string
	StatusCode int
	Allowed    bool
	DurationMs int64
}

// --- SyncLogger ---

type SyncLogger struct{ db *sql.DB }

func NewSync(db *sql.DB) *SyncLogger { return &SyncLogger{db: db} }

func (s *SyncLogger) Log(e Entry) {
	// Blocks the goroutine (and therefore the client response) until
	// Postgres acknowledges the write.
	s.db.Exec(`
		INSERT INTO request_logs (client_id, path, method, status_code, allowed, duration_ms)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		e.ClientID, e.Path, e.Method, e.StatusCode, e.Allowed, e.DurationMs,
	)
}

// --- AsyncLogger ---

const (
	maxBatch      = 500                   // flush when this many entries are buffered...
	flushInterval = 50 * time.Millisecond // ...or at least this often
)

type AsyncLogger struct {
	db      *sql.DB
	ch      chan Entry
	stopCh  chan struct{}
	stopped chan struct{}
	dropped atomic.Int64
}

func NewAsync(db *sql.DB, bufferSize int) *AsyncLogger {
	return &AsyncLogger{
		db:      db,
		ch:      make(chan Entry, bufferSize),
		stopCh:  make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

// Dropped returns how many entries were discarded (buffer full or write error).
func (a *AsyncLogger) Dropped() int64 { return a.dropped.Load() }

// Start launches the background writer. It batches entries and writes each
// batch with a single COPY, which is far cheaper than one INSERT per request.
func (a *AsyncLogger) Start() {
	go func() {
		defer close(a.stopped)
		batch := make([]Entry, 0, maxBatch)
		tick := time.NewTicker(flushInterval)
		defer tick.Stop()
		var reported int64
		ticks := 0
		for {
			select {
			case e := <-a.ch:
				batch = append(batch, e)
				if len(batch) >= maxBatch {
					a.flush(batch)
					batch = batch[:0]
				}
			case <-tick.C:
				if len(batch) > 0 {
					a.flush(batch)
					batch = batch[:0]
				}
				if ticks++; ticks%200 == 0 { // every ~10s
					if d := a.dropped.Load(); d != reported {
						log.Printf("async logger: %d entries dropped so far", d)
						reported = d
					}
				}
			case <-a.stopCh:
				// Drain whatever is still buffered, then exit.
				for {
					select {
					case e := <-a.ch:
						batch = append(batch, e)
						if len(batch) >= maxBatch {
							a.flush(batch)
							batch = batch[:0]
						}
					default:
						a.flush(batch)
						log.Printf("async logger stopped; %d entries dropped in total", a.dropped.Load())
						return
					}
				}
			}
		}
	}()
}

func (a *AsyncLogger) flush(batch []Entry) {
	if len(batch) == 0 {
		return
	}
	if err := a.copyBatch(batch); err != nil {
		a.dropped.Add(int64(len(batch)))
		log.Printf("async logger: batch of %d failed: %v", len(batch), err)
	}
}

func (a *AsyncLogger) copyBatch(batch []Entry) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(pq.CopyIn("request_logs",
		"client_id", "path", "method", "status_code", "allowed", "duration_ms"))
	if err != nil {
		tx.Rollback()
		return err
	}
	for _, e := range batch {
		if _, err := stmt.Exec(e.ClientID, e.Path, e.Method, e.StatusCode, e.Allowed, e.DurationMs); err != nil {
			stmt.Close()
			tx.Rollback()
			return err
		}
	}
	if _, err := stmt.Exec(); err != nil { // flush the COPY
		stmt.Close()
		tx.Rollback()
		return err
	}
	if err := stmt.Close(); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Stop signals the background goroutine to drain and exit.
func (a *AsyncLogger) Stop() {
	close(a.stopCh)
	<-a.stopped
}

// Log is non-blocking: it drops the entry into the channel and returns
// immediately. If the channel is full (backpressure), the entry is dropped
// (and counted) rather than blocking the request: observability must not
// degrade client-facing latency. Trade-off: async logging is best-effort
// (entries can be lost on overflow or a hard crash); sync logging is durable
// but slower.
func (a *AsyncLogger) Log(e Entry) {
	select {
	case a.ch <- e:
	default:
		a.dropped.Add(1)
	}
}

// --- Gin middleware ---

func Middleware(logger Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		// Capture before c.Next(): the proxy rewrites URL.Path (strips /v1/proxy),
		// so reading it afterwards would log the upstream path, not the client's.
		path := c.Request.URL.Path
		c.Next()

		allowed := c.Writer.Status() != http.StatusTooManyRequests
		logger.Log(Entry{
			ClientID:   c.GetString("client_id"),
			Path:       path,
			Method:     c.Request.Method,
			StatusCode: c.Writer.Status(),
			Allowed:    allowed,
			DurationMs: time.Since(start).Milliseconds(),
		})
	}
}

// LogsHandler returns a client's own request history.
func LogsHandler(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetString("client_id")
		limit := 50
		if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 {
			limit = v
			if limit > 200 {
				limit = 200
			}
		}
		rows, err := db.Query(`
			SELECT path, method, status_code, allowed, duration_ms, created_at
			FROM request_logs WHERE client_id=$1
			ORDER BY created_at DESC LIMIT $2`, id, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
			return
		}
		defer rows.Close()
		logs := []gin.H{}
		for rows.Next() {
			var path, method string
			var status int
			var allowed bool
			var durationMs int64
			var createdAt string
			if err := rows.Scan(&path, &method, &status, &allowed, &durationMs, &createdAt); err != nil {
				continue
			}
			logs = append(logs, gin.H{
				"path": path, "method": method, "status": status,
				"allowed": allowed, "duration_ms": durationMs, "at": createdAt,
			})
		}
		c.JSON(http.StatusOK, gin.H{"logs": logs})
	}
}
