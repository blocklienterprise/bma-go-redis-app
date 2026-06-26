// Internal publish webhook — called by WordPress (the bp_messages_message_after_save
// hook, read-receipt hooks, etc.) to fan an event out to a thread's subscribers.
//
//	POST /internal/publish
//	X-Internal-Token: {INTERNAL_PUBLISH_TOKEN}
//	{"thread_id":123,"type":"message.new","user_id":45,"data":{...}}
//
// WordPress stays the source of truth; this only broadcasts. Messages sent from
// the website therefore also appear live in the app.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"time"
)

func (s *server) handleInternalPublish(w http.ResponseWriter, r *http.Request) {
	if s.internalToken == "" || r.Header.Get("X-Internal-Token") != s.internalToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var in struct {
		ThreadID int             `json:"thread_id"`
		Type     string          `json:"type"`
		UserID   int             `json:"user_id"`
		Data     json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || in.ThreadID == 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if in.Type == "" {
		in.Type = "message.new"
	}

	ev := mustJSON(event{
		Type:     in.Type,
		ThreadID: in.ThreadID,
		UserID:   in.UserID,
		Data:     in.Data,
		TS:       time.Now().Unix(),
	})
	if err := s.rt.publish(r.Context(), in.ThreadID, ev); err != nil {
		http.Error(w, "publish failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
