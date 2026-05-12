package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"

	"github.com/redis/go-redis/v9"
)

var rdb *redis.Client

func main() {
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://localhost:6379"
	}

	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Fatalf("invalid REDIS_URL: %v", err)
	}

	rdb = redis.NewClient(opts)

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("failed to connect to redis: %v", err)
	}

	http.HandleFunc("GET /{$}", rootHandler)
	http.HandleFunc("GET /kv/{key}", getHandler)
	http.HandleFunc("PUT /kv/{key}", putHandler)
	http.HandleFunc("DELETE /kv/{key}", deleteHandler)

	log.Println("listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func rootHandler(w http.ResponseWriter, r *http.Request) {
	endpoints := map[string]any{
		"endpoints": []map[string]string{
			{"method": "GET", "path": "/kv/{key}", "description": "Get a value by key"},
			{"method": "PUT", "path": "/kv/{key}", "description": "Set a value by key"},
			{"method": "DELETE", "path": "/kv/{key}", "description": "Delete a value by key"},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(endpoints)
}

func getHandler(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	val, err := rdb.Get(r.Context(), key).Result()
	if err == redis.Nil {
		http.Error(w, "key not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"key": key, "value": val})
}

func putHandler(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if err := rdb.Set(r.Context(), key, body.Value, 0).Err(); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"key": key, "value": body.Value})
}

func deleteHandler(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	deleted, err := rdb.Del(r.Context(), key).Result()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if deleted == 0 {
		http.Error(w, "key not found", http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
