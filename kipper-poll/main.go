package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
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

	log.Printf("kipper-poll starting: trigger=%s target=%s interval=%s", triggerType, targetURL, pollInterval)

	ctx := context.Background()

	switch triggerType {
	case "postgres", "mysql":
		pollSQL(ctx, triggerType, targetURL, pollInterval)
	case "redis":
		pollRedis(ctx, targetURL, pollInterval)
	case "minio":
		listenMinIO(targetURL)
	default:
		log.Fatalf("unknown trigger type: %s", triggerType)
	}
}

func pollSQL(ctx context.Context, driver, targetURL string, interval time.Duration) {
	dsn := os.Getenv("KIPPER_SOURCE_URL")
	query := os.Getenv("KIPPER_QUERY")
	markDone := os.Getenv("KIPPER_MARK_DONE")

	if dsn == "" || query == "" {
		log.Fatal("KIPPER_SOURCE_URL and KIPPER_QUERY are required for SQL triggers")
	}

	dbDriver := "postgres"
	if driver == "mysql" {
		dbDriver = "mysql"
		// Convert postgres-style URL to MySQL DSN if needed
		dsn = convertMySQLDSN(dsn)
	}

	db, err := sql.Open(dbDriver, dsn)
	if err != nil {
		log.Fatalf("connecting to database: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Wait for database to be ready
	for i := 0; i < 30; i++ {
		if err := db.PingContext(ctx); err == nil {
			break
		}
		time.Sleep(time.Second)
	}

	log.Printf("connected to %s, polling with: %s", driver, query)

	for {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			log.Printf("query error: %v", err)
			time.Sleep(interval)
			continue
		}

		columns, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			time.Sleep(interval)
			continue
		}

		for rows.Next() {
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

			if err := sendEvent(targetURL, event); err != nil {
				log.Printf("failed to send event: %v", err)
				continue
			}

			// Mark as done if configured
			if markDone != "" {
				execMarkDone(db, markDone, event)
			}
		}
		_ = rows.Close()

		time.Sleep(interval)
	}
}

func pollRedis(ctx context.Context, targetURL string, interval time.Duration) {
	redisAddr := os.Getenv("KIPPER_SOURCE_URL")
	listName := os.Getenv("KIPPER_REDIS_LIST")

	if redisAddr == "" || listName == "" {
		log.Fatal("KIPPER_SOURCE_URL and KIPPER_REDIS_LIST are required for Redis triggers")
	}

	// Strip redis:// prefix
	addr := strings.TrimPrefix(redisAddr, "redis://")

	log.Printf("connected to Redis at %s, watching list: %s", addr, listName)

	for {
		// Use raw RESP protocol for LPOP — no external dependency needed
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			log.Printf("Redis connection error: %v", err)
			time.Sleep(interval)
			continue
		}

		// Send LPOP command in RESP format
		cmd := fmt.Sprintf("*2\r\n$4\r\nLPOP\r\n$%d\r\n%s\r\n", len(listName), listName)
		_, _ = conn.Write([]byte(cmd))

		// Read response
		buf := make([]byte, 4096)
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := conn.Read(buf)
		_ = conn.Close()

		if err != nil || n == 0 {
			time.Sleep(interval)
			continue
		}

		response := string(buf[:n])

		// RESP nil bulk string = "$-1\r\n" (empty list)
		if strings.HasPrefix(response, "$-1") {
			time.Sleep(interval)
			continue
		}

		// Parse bulk string: $<length>\r\n<data>\r\n
		if strings.HasPrefix(response, "$") {
			parts := strings.SplitN(response, "\r\n", 3)
			if len(parts) >= 2 {
				data := parts[1]

				// Try to parse as JSON
				var event interface{}
				if json.Unmarshal([]byte(data), &event) == nil {
					if err := sendEvent(targetURL, event); err != nil {
						log.Printf("failed to send event: %v", err)
					} else {
						log.Printf("processed Redis event from %s", listName)
					}
				} else {
					// Not JSON — send as string
					if err := sendEvent(targetURL, map[string]string{"data": data}); err != nil {
						log.Printf("failed to send event: %v", err)
					}
				}
			}
		}

		// Don't sleep between items — process quickly
		continue
	}
}

func listenMinIO(targetURL string) {
	// MinIO sends bucket notifications as HTTP webhooks
	// kipper-poll runs a small HTTP server that receives them and forwards to the function
	port := os.Getenv("KIPPER_MINIO_WEBHOOK_PORT")
	if port == "" {
		port = "9090"
	}

	log.Printf("listening for MinIO bucket notifications on :%s", port)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
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

		if err := sendEvent(targetURL, event); err != nil {
			log.Printf("failed to forward MinIO event: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
	})

	if err := http.ListenAndServe(":"+port, nil); err != nil { //nolint:gosec
		log.Fatalf("webhook server failed: %v", err)
	}
}

func sendEvent(targetURL string, event interface{}) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshalling event: %w", err)
	}

	resp, err := http.Post(targetURL, "application/json", bytes.NewReader(body)) //nolint:gosec
	if err != nil {
		return fmt.Errorf("posting event: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("function returned %d", resp.StatusCode)
	}

	return nil
}

func execMarkDone(db *sql.DB, template string, event map[string]interface{}) {
	query := template
	for k, v := range event {
		placeholder := "{{" + k + "}}"
		query = strings.ReplaceAll(query, placeholder, fmt.Sprintf("%v", v))
	}

	if _, err := db.Exec(query); err != nil {
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
