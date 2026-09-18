package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

type Server struct {
	svc *Service
}

func NewServer(svc *Service) *Server { return &Server{svc: svc} }

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/v1/accounts", s.postAccount)
	mux.HandleFunc("/v1/fx/snapshots", s.postFX)
	mux.HandleFunc("/v1/fx/pending", s.getPendingFX)
	mux.HandleFunc("/v1/fx/", s.fxSubrouter)
	mux.HandleFunc("/v1/events", s.postEvent)
	mux.HandleFunc("/v1/accounts/", s.accountSubrouter)
	mux.HandleFunc("/v1/disputes/", s.disputeSubrouter)
	return mux
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrInvalid), errors.Is(err, ErrFxUnavailable):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return false
	}
	return true
}

func requireAgent(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.Header.Get("X-Role") != "agent" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "agent role required (X-Role: agent)"})
		return "", false
	}
	id := r.Header.Get("X-User-Id")
	if id == "" {
		id = "anonymous-agent"
	}
	return id, true
}

func pathTail(prefix, path string) string {
	return strings.TrimPrefix(path, prefix)
}

// ---------- accounts / fx / events ----------

func (s *Server) postAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	var req CreateAccountReq
	if !decode(w, r, &req) {
		return
	}
	a, err := s.svc.CreateAccount(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

type fxReq struct {
	From        string `json:"from_currency"`
	To          string `json:"to_currency"`
	Source      string `json:"source"`
	Rate        string `json:"rate"`
	EffectiveAt string `json:"effective_at"`
}

func (s *Server) postFX(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	var req fxReq
	if !decode(w, r, &req) {
		return
	}
	at := time.Now()
	if req.EffectiveAt != "" {
		t, err := time.Parse(time.RFC3339, req.EffectiveAt)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "effective_at must be RFC3339"})
			return
		}
		at = t
	}
	snap, err := s.svc.AddFXSnapshot(req.From, req.To, req.Source, req.Rate, at)
	if err != nil {
		writeErr(w, err)
		return
	}
	code := http.StatusCreated
	if snap.Status == statusPending {
		code = http.StatusAccepted // 202：已收讫但待人工复核
	}
	writeJSON(w, code, snap)
}

func (s *Server) getPendingFX(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET only"})
		return
	}
	writeJSON(w, http.StatusOK, s.svc.PendingFX())
}

func (s *Server) fxSubrouter(w http.ResponseWriter, r *http.Request) {
	// /v1/fx/{snapshotID}/review
	tail := pathTail("/v1/fx/", r.URL.Path)
	parts := strings.Split(tail, "/")
	if len(parts) == 2 && parts[1] == "review" && r.Method == http.MethodPost {
		agentID, ok := requireAgent(w, r)
		if !ok {
			return
		}
		var body struct {
			Decision string `json:"decision"`
		}
		if !decode(w, r, &body) {
			return
		}
		snap, err := s.svc.ReviewFX(parts[0], body.Decision, agentID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, snap)
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown path"})
}

func (s *Server) postEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	var ev CardEvent
	if !decode(w, r, &ev) {
		return
	}
	res, err := s.svc.Ingest(ev)
	if err != nil {
		writeErr(w, err)
		return
	}
	code := http.StatusOK
	switch res.Status {
	case statusAccepted:
		code = http.StatusCreated
	case statusDeclined:
		code = http.StatusUnprocessableEntity // 422：报文合法但业务拒绝
	case statusDuplicate:
		code = http.StatusOK
	}
	writeJSON(w, code, res)
}

// ---------- account-scoped subroutes ----------

func (s *Server) accountSubrouter(w http.ResponseWriter, r *http.Request) {
	tail := pathTail("/v1/accounts/", r.URL.Path)
	parts := strings.Split(tail, "/")
	if len(parts) < 2 || parts[0] == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown path"})
		return
	}
	accountID := parts[0]
	resource := strings.Join(parts[1:], "/")
	switch {
	case resource == "balance" && r.Method == http.MethodGet:
		v, err := s.svc.GetBalanceView(accountID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	case resource == "details" && r.Method == http.MethodGet:
		details, err := s.svc.ListDetails(accountID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, details)
	case resource == "notifications" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.svc.Notifications(accountID))
	case resource == "conversions" && r.Method == http.MethodPost:
		var body struct {
			Subject      string `json:"subject"`
			FromCurrency string `json:"from_currency"`
			Amount       int64  `json:"amount_minor"`
		}
		if !decode(w, r, &body) {
			return
		}
		b, err := s.svc.FixConversion(accountID, body.Subject, body.FromCurrency, body.Amount)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, b)
	case resource == "disputes" && r.Method == http.MethodPost:
		var body struct {
			EventID    string `json:"event_id"`
			Reason     string `json:"reason"`
			CustomerID string `json:"customer_id"`
		}
		if !decode(w, r, &body) {
			return
		}
		d, err := s.svc.OpenDispute(accountID, body.EventID, body.Reason, body.CustomerID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, d)
	case resource == "export" && r.Method == http.MethodGet:
		agentID, ok := requireAgent(w, r)
		if !ok {
			return
		}
		rep, err := s.svc.ExportAccount(accountID, agentID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, rep)
	case resource == "verify" && r.Method == http.MethodGet:
		e, a, at := s.svc.VerifyChain()
		writeJSON(w, http.StatusOK, map[string]any{
			"entries_chain_ok": e, "audit_chain_ok": a, "broken_at_seq": at,
		})
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown path"})
	}
}

// ---------- disputes ----------

func (s *Server) disputeSubrouter(w http.ResponseWriter, r *http.Request) {
	tail := pathTail("/v1/disputes/", r.URL.Path)
	parts := strings.Split(tail, "/")
	if len(parts) < 1 || parts[0] == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown path"})
		return
	}
	id := parts[0]
	resource := ""
	if len(parts) > 1 {
		resource = parts[1]
	}
	switch {
	case resource == "" && r.Method == http.MethodGet:
		d, err := s.svc.Dispute(id)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, d)
	case resource == "conclusions" && r.Method == http.MethodPost:
		agentID, ok := requireAgent(w, r)
		if !ok {
			return
		}
		var body struct {
			Content string `json:"content"`
		}
		if !decode(w, r, &body) {
			return
		}
		d, err := s.svc.AddDisputeConclusion(id, agentID, body.Content)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, d)
	case resource == "resolve" && r.Method == http.MethodPost:
		agentID, ok := requireAgent(w, r)
		if !ok {
			return
		}
		var body struct {
			Decision   string `json:"decision"`
			Conclusion string `json:"conclusion"`
		}
		if !decode(w, r, &body) {
			return
		}
		d, err := s.svc.ResolveDispute(id, body.Decision, agentID, body.Conclusion)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, d)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown path"})
	}
}
