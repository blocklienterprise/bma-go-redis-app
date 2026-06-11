// Blockli Cache Proxy — Go HTTP server that wraps Redis for Bunny Edge Script.
//
// The Bunny Edge Script calls this proxy via HTTP (fetch).
// This proxy handles all Redis operations internally, connecting to Redis
// via the REDIS_URL environment variable (defaults to redis://localhost:6379).
//
// Endpoints:
//
//	GET  /health                    — liveness + Redis ping
//	GET  /cache?key={k}             — retrieve a cached value
//	PUT  /cache?key={k}&ttl={s}     — store a value with TTL
//	POST /cache/purge               — SCAN + DEL by glob pattern
//
// Auth: every request (except /health) must carry X-Cache-Token matching CACHE_API_TOKEN env var.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisConnectWithRetry tries to ping Redis up to maxAttempts times,
// waiting delay between each attempt. Exits the process on final failure.
func redisConnectWithRetry(client *redis.Client, maxAttempts int, delay time.Duration) {
	for i := 1; i <= maxAttempts; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := client.Ping(ctx).Err()
		cancel()
		if err == nil {
			log.Printf("Redis connected (attempt %d/%d)", i, maxAttempts)
			return
		}
		if i == maxAttempts {
			log.Fatalf("Redis unavailable after %d attempts: %v", maxAttempts, err)
		}
		log.Printf("Redis not ready (attempt %d/%d): %v — retrying in %s", i, maxAttempts, err, delay)
		time.Sleep(delay)
	}
}

// ---------------------------------------------------------------------------
// Globals
// ---------------------------------------------------------------------------

var (
	rdb      *redis.Client
	apiToken string
)

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

func main() {
	redisURL := getEnv("REDIS_URL", "redis://localhost:6379")
	apiToken = getEnv("CACHE_API_TOKEN", "")
	port := getEnv("PORT", "8080")

	if apiToken == "" {
		log.Println("WARNING: CACHE_API_TOKEN is not set. All requests will be accepted without auth.")
	}

	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Fatalf("Invalid REDIS_URL %q: %v", redisURL, err)
	}

	// Sensible timeouts — Magic Container pods are co-located so Redis RTT is <1 ms.
	opts.DialTimeout = 5 * time.Second
	opts.ReadTimeout = 3 * time.Second
	opts.WriteTimeout = 3 * time.Second

	rdb = redis.NewClient(opts)

	// Retry up to 20 times (30 s total) — Redis sidecar may not be ready immediately.
	redisConnectWithRetry(rdb, 20, 1500*time.Millisecond)
	log.Printf("Redis ready at %s", redisURL)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /cache", auth(handleGet))
	mux.HandleFunc("PUT /cache", auth(handleSet))
	mux.HandleFunc("POST /cache/purge", auth(handlePurge))

	log.Printf("Cache proxy listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, corsMiddleware(mux)))
}

// ---------------------------------------------------------------------------
// CORS middleware
// ---------------------------------------------------------------------------

// corsMiddleware adds permissive CORS headers so the Blockli Studio web app
// (and any other browser client) can call the proxy directly.
// Security is enforced by the X-Cache-Token auth layer, not by Origin restriction.
func corsMiddleware(next http.Handler) http.Handler {
	allowed := getEnv("CORS_ALLOWED_ORIGINS", "*")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if allowed == "*" {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		} else if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Cache-Token")
		w.Header().Set("Access-Control-Max-Age", "86400")

		// Handle preflight
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Auth middleware
// ---------------------------------------------------------------------------

func auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if apiToken != "" && r.Header.Get("X-Cache-Token") != apiToken {
			jsonError(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// GET /health
// Always returns 200 so the Docker/Bunny health check never kills the container
// due to a transient Redis hiccup. Redis reachability is still reported in the
// body for observability.
func handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	redisOk := rdb.Ping(ctx).Err() == nil
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": "ok",
		"redis":  redisOk,
	})
}

// GET /cache?key={key}
// Returns 200 with raw JSON body, 404 if not found.
func handleGet(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		jsonError(w, "missing key", http.StatusBadRequest)
		return
	}

	val, err := rdb.Get(r.Context(), key).Bytes()
	if err == redis.Nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("Redis GET error for key %q: %v", key, err)
		jsonError(w, "redis error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(val)
}

// PUT /cache?key={key}&ttl={seconds}
// Body: raw JSON. Stores in Redis with TTL. Returns 204 on success.
func handleSet(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		jsonError(w, "missing key", http.StatusBadRequest)
		return
	}

	ttl := 300 // default 5 min
	if s := r.URL.Query().Get("ttl"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			ttl = n
		}
	}

	// Cap at 10 MB to prevent abuse.
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil || len(body) == 0 {
		jsonError(w, "missing or unreadable body", http.StatusBadRequest)
		return
	}

	if err := rdb.Set(r.Context(), key, body, time.Duration(ttl)*time.Second).Err(); err != nil {
		log.Printf("Redis SET error for key %q: %v", key, err)
		jsonError(w, "redis error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// POST /cache/purge
// Body: {"pattern":"blockli:prod:1:*"}
// Returns 200 {"ok":true,"deleted":N,"pattern":"..."}
func handlePurge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Pattern string `json:"pattern"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Pattern == "" {
		jsonError(w, "missing or invalid pattern", http.StatusBadRequest)
		return
	}

	deleted, err := scanAndDelete(r.Context(), req.Pattern)
	if err != nil {
		log.Printf("Purge error for pattern %q: %v", req.Pattern, err)
		jsonError(w, "purge failed", http.StatusInternalServerError)
		return
	}

	log.Printf("Purged %d keys matching %q", deleted, req.Pattern)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"deleted": deleted,
		"pattern": req.Pattern,
	})
}

// ---------------------------------------------------------------------------
// Redis helpers
// ---------------------------------------------------------------------------

// scanAndDelete iterates via SCAN (non-blocking cursor loop) and deletes matching keys.
// Using SCAN instead of KEYS avoids blocking Redis on large keyspaces.
func scanAndDelete(ctx context.Context, pattern string) (int64, error) {
	var cursor uint64
	var deleted int64

	for {
		keys, nextCursor, err := rdb.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return deleted, fmt.Errorf("SCAN cursor=%d: %w", cursor, err)
		}
		if len(keys) > 0 {
			n, err := rdb.Del(ctx, keys...).Result()
			if err != nil {
				return deleted, fmt.Errorf("DEL: %w", err)
			}
			deleted += n
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return deleted, nil
}

// ---------------------------------------------------------------------------
// Utilities
// ---------------------------------------------------------------------------

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
