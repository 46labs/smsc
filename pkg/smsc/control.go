package smsc

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/linxGnu/gosmpp/data"
)

// Control is the HTTP API tests drive the mock with:
//
//	GET  /healthz
//	GET  /mock/pdus?system_id=     PDUs received
//	POST /mock/mo                  {system_id, source, destination, text}
//	POST /mock/submit-errors       {statuses: [88]} for the next submits
//	PUT  /mock/receipts            {state, delay_ms, none}
//	POST /mock/drop                {system_id}
//	PUT  /mock/responses           {hold: true} leaves submits unanswered
//	POST /mock/responses/release   answers them and stops holding
//	GET  /mock/responses           {held: N}
//	POST /mock/reset
func (s *Server) Control() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /mock/pdus", func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusOK, s.Records(r.URL.Query().Get("system_id")))
	})
	mux.HandleFunc("POST /mock/mo", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			SystemID    string `json:"system_id"`
			Source      string `json:"source"`
			Destination string `json:"destination"`
			Text        string `json:"text"`
		}
		if !decode(w, r, &in) {
			return
		}
		statuses, err := s.MO(r.Context(), in.SystemID, in.Source, in.Destination, in.Text)
		if err != nil {
			reply(w, http.StatusConflict, map[string]any{"error": err.Error(), "statuses": statuses})
			return
		}
		reply(w, http.StatusOK, map[string]any{"statuses": statuses})
	})
	mux.HandleFunc("POST /mock/submit-errors", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Statuses []uint32 `json:"statuses"`
		}
		if !decode(w, r, &in) {
			return
		}
		st := make([]data.CommandStatusType, len(in.Statuses))
		for i, v := range in.Statuses {
			st[i] = data.CommandStatusType(v)
		}
		s.FailSubmits(st)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT /mock/receipts", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			State   string `json:"state"`
			DelayMS int    `json:"delay_ms"`
			None    bool   `json:"none"`
		}
		if !decode(w, r, &in) {
			return
		}
		if _, ok := messageStates[in.State]; !ok && !in.None {
			reply(w, http.StatusBadRequest, map[string]string{"error": "unknown state " + in.State})
			return
		}
		s.SetReceipts(Receipts{State: in.State, Delay: time.Duration(in.DelayMS) * time.Millisecond, None: in.None})
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /mock/drop", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			SystemID string `json:"system_id"`
		}
		if !decode(w, r, &in) {
			return
		}
		reply(w, http.StatusOK, map[string]int{"dropped": s.Drop(in.SystemID)})
	})
	mux.HandleFunc("PUT /mock/responses", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Hold bool `json:"hold"`
		}
		if !decode(w, r, &in) {
			return
		}
		if in.Hold {
			s.Hold()
		} else {
			s.Release()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /mock/responses/release", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusOK, map[string]int{"released": s.Release()})
	})
	mux.HandleFunc("GET /mock/responses", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusOK, map[string]int{"held": s.Held()})
	})
	mux.HandleFunc("POST /mock/reset", func(w http.ResponseWriter, _ *http.Request) {
		s.Reset()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		reply(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return false
	}
	return true
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
