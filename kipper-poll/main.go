package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

func main() {
	triggerType := os.Getenv("KIPPER_TRIGGER")
	targetURL := os.Getenv("KIPPER_TARGET_URL")
	if targetURL == "" {
		targetURL = "http://localhost:8080/event"
	}

	pollInterval := 5 * time.Second
	if v := os.Getenv("KIPPER_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			pollInterval = d
		}
	}

	switch triggerType {
	case "postgres", "mysql", "redis", "minio":
	default:
		log.Fatalf("unknown trigger type: %s", triggerType)
	}

	log.Printf("kipper-poll starting: trigger=%s target=%s interval=%s", triggerType, targetURL, pollInterval)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	switch triggerType {
	case "postgres", "mysql":
		pollSQL(ctx, triggerType, targetURL, pollInterval)
	case "redis":
		pollRedis(ctx, targetURL, pollInterval)
	case "minio":
		listenMinIO(ctx, targetURL)
	}
}

func pollSQL(ctx context.Context, driver, targetURL string, interval time.Duration) {
	dsn := os.Getenv("KIPPER_SOURCE_URL")
	query := os.Getenv("KIPPER_QUERY")
	markDone := os.Getenv("KIPPER_MARK_DONE")

	if dsn == "" || query == "" {
		log.Fatal("KIPPER_SOURCE_URL and KIPPER_QUERY are required for SQL triggers")
	}

	var markDoneQuery *markDoneTemplate
	if markDone != "" {
		parsed, err := parseMarkDone(driver, markDone)
		if err != nil {
			log.Fatalf("KIPPER_MARK_DONE: %v", err)
		}
		markDoneQuery = parsed
	}

	dbDriver := "postgres"
	if driver == "mysql" {
		dbDriver = "mysql"
		converted, err := mysqlDSN(dsn)
		if err != nil {
			log.Fatalf("KIPPER_SOURCE_URL is not a valid MySQL URL: %v", err)
		}
		dsn = converted
	}

	db, err := sql.Open(dbDriver, dsn)
	if err != nil {
		log.Fatalf("connecting to database: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Allow the database to start before entering the polling loop.
	for i := 0; i < 30; i++ {
		if err := db.PingContext(ctx); err == nil {
			break
		}
		if !wait(ctx, time.Second) {
			return
		}
	}

	log.Printf("connected to %s, polling with: %s", driver, query)

	for {
		rows, err := db.QueryContext(ctx, query) //nolint:gosec // G701: the operator's own configured query
		if err != nil {
			log.Printf("query error: %v", err)
			if !wait(ctx, interval) {
				return
			}
			continue
		}

		columns, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			if !wait(ctx, interval) {
				return
			}
			continue
		}

		for ctx.Err() == nil && rows.Next() {
			values := make([]interface{}, len(columns))
			valuePtrs := make([]interface{}, len(columns))
			for i := range values {
				valuePtrs[i] = &values[i]
			}

			if err := rows.Scan(valuePtrs...); err != nil {
				log.Printf("scan error: %v", err)
				continue
			}

			event := make(map[string]interface{})
			for i, col := range columns {
				val := values[i]
				if b, ok := val.([]byte); ok {
					event[col] = string(b)
				} else {
					event[col] = val
				}
			}

			if err := sendEvent(ctx, targetURL, event); err != nil {
				log.Printf("failed to send event: %v", err)
				continue
			}

			if markDoneQuery != nil {
				execMarkDone(ctx, db, markDoneQuery, event)
			}
		}
		_ = rows.Close()

		if !wait(ctx, interval) {
			return
		}
	}
}

func pollRedis(ctx context.Context, targetURL string, interval time.Duration) {
	redisAddr := os.Getenv("KIPPER_SOURCE_URL")
	listName := os.Getenv("KIPPER_REDIS_LIST")

	if redisAddr == "" || listName == "" {
		log.Fatal("KIPPER_SOURCE_URL and KIPPER_REDIS_LIST are required for Redis triggers")
	}

	addr := strings.TrimPrefix(redisAddr, "redis://")

	log.Printf("connected to Redis at %s, watching list: %s", addr, listName)

	dialer := net.Dialer{Timeout: 5 * time.Second}
	for ctx.Err() == nil {
		conn, err := dialer.DialContext(ctx, "tcp", addr) //nolint:gosec // G704: the operator's own configured Redis address
		if err != nil {
			log.Printf("Redis connection error: %v", err)
			if !wait(ctx, interval) {
				return
			}
			continue
		}

		// Encode LPOP directly in RESP to avoid a Redis client dependency.
		cmd := fmt.Sprintf("*2\r\n$4\r\nLPOP\r\n$%d\r\n%s\r\n", len(listName), listName)
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = conn.Write([]byte(cmd))

		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		_ = conn.Close()

		if err != nil || n == 0 {
			if !wait(ctx, interval) {
				return
			}
			continue
		}

		response := string(buf[:n])

		// RESP nil bulk string = "$-1\r\n" (empty list)
		if strings.HasPrefix(response, "$-1") {
			if !wait(ctx, interval) {
				return
			}
			continue
		}

		// Parse bulk string: $<length>\r\n<data>\r\n
		if strings.HasPrefix(response, "$") {
			parts := strings.SplitN(response, "\r\n", 3)
			if len(parts) >= 2 {
				data := parts[1]

				var event interface{}
				if json.Unmarshal([]byte(data), &event) == nil {
					if err := sendEvent(ctx, targetURL, event); err != nil {
						log.Printf("failed to send event: %v", err)
					} else {
						log.Printf("processed Redis event from %s", listName)
					}
				} else {
					if err := sendEvent(ctx, targetURL, map[string]string{"data": data}); err != nil {
						log.Printf("failed to send event: %v", err)
					}
				}
			}
		}

		// Drain queued items immediately; idle and error paths wait above.
	}
}

// wait returns true when the timer fires, or false when cancellation is selected.
func wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func listenMinIO(ctx context.Context, targetURL string) {
	port := os.Getenv("KIPPER_MINIO_WEBHOOK_PORT")
	if port == "" {
		port = "9090"
	}

	log.Printf("listening for MinIO bucket notifications on :%s", port)
	if err := serveMinIO(ctx, ":"+port, targetURL); err != nil {
		log.Fatalf("webhook server failed: %v", err)
	}
}

var webhookShutdownGrace = 10 * time.Second

// serveMinIO forwards MinIO webhooks to the function until ctx ends, then
// lets requests in flight finish for webhookShutdownGrace before closing them.
func serveMinIO(ctx context.Context, addr, targetURL string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusOK)
			return
		}

		var event interface{}
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			log.Printf("failed to decode MinIO event: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if err := sendEvent(r.Context(), targetURL, event); err != nil {
			log.Printf("failed to forward MinIO event: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), webhookShutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("closing webhook requests still open after %s", webhookShutdownGrace)
			done <- srv.Close()
			return
		}
		done <- nil
	}()

	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return <-done
}

func sendEvent(ctx context.Context, targetURL string, event interface{}) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshalling event: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body)) //nolint:gosec // G704: the operator's own configured function URL
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: the operator's own configured function URL
	if err != nil {
		return fmt.Errorf("posting event: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("function returned %d", resp.StatusCode)
	}

	return nil
}

// markDoneTimeout limits the update attempt after successful delivery, including
// during shutdown, to reduce duplicate delivery on restart.
const markDoneTimeout = 10 * time.Second

func execMarkDone(ctx context.Context, db *sql.DB, template *markDoneTemplate, event map[string]interface{}) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markDoneTimeout)
	defer cancel()
	query, args := template.bind(event)
	if _, err := db.ExecContext(ctx, query, args...); err != nil { //nolint:gosec // G701: the operator's template; row values are bound as parameters
		log.Printf("mark-done error: %v", err)
	}
}

func convertMySQLDSN(url string) string {
	// Convert mysql://user:pass@host:port/db to user:pass@tcp(host:port)/db
	url = strings.TrimPrefix(url, "mysql://")
	parts := strings.SplitN(url, "@", 2)
	if len(parts) != 2 {
		return url
	}
	hostDB := parts[1]
	hostParts := strings.SplitN(hostDB, "/", 2)
	if len(hostParts) != 2 {
		return url
	}
	return fmt.Sprintf("%s@tcp(%s)/%s", parts[0], hostParts[0], hostParts[1])
}
