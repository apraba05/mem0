// Mini memory layer: extract → compress → retrieve.
//
// Redis schema:  mem:{user_id}  →  LIST of compact fact strings
//
// POST /ingest   chat turn → Bedrock extracts a 1–2 sentence fact → RPUSH
// GET  /context  pull facts for user_id, keyword-rank against query, return top-k
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/redis/go-redis/v9"
)

const (
	defaultListen = ":8080"
	defaultModel  = "anthropic.claude-3-haiku-20240307-v1:0"
	extractSystem = "Extract ONE durable user fact from the chat turn as a single " +
		"1-2 sentence statement in third person. Prefer preferences, identity, " +
		"constraints. If nothing worth remembering, reply with exactly: NONE"
)

type server struct {
	rdb   *redis.Client
	br    *bedrockruntime.Client
	model string
}

type ingestReq struct {
	UserID  string `json:"user_id"`
	Role    string `json:"role"`
	Message string `json:"message"`
}

type ingestResp struct {
	UserID string `json:"user_id"`
	Fact   string `json:"fact"` // empty when model said NONE
	Key    string `json:"key"`
	Stored bool   `json:"stored"`
	Ms     int64  `json:"latency_ms"`
}

type contextResp struct {
	UserID  string   `json:"user_id"`
	Query   string   `json:"query"`
	Facts   []string `json:"facts"`
	Context string   `json:"context"` // newline-joined top-k, ready to inject
	Total   int      `json:"total_stored"`
	Ms      int64    `json:"latency_ms"`
}

func main() {
	redisAddr := env("REDIS_ADDR", "127.0.0.1:6379")
	listen := env("LISTEN", defaultListen)
	model := env("BEDROCK_MODEL", defaultModel)
	region := env("AWS_REGION", env("AWS_DEFAULT_REGION", "us-east-1"))

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("redis %s: %v", redisAddr, err)
	}

	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(region))
	if err != nil {
		log.Fatalf("aws config: %v", err)
	}
	s := &server{
		rdb:   rdb,
		br:    bedrockruntime.NewFromConfig(cfg),
		model: model,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"ok": "true"})
	})
	mux.HandleFunc("/ingest", s.handleIngest)
	mux.HandleFunc("/context", s.handleContext)

	log.Printf("memory layer on %s  redis=%s  model=%s", listen, redisAddr, model)
	log.Fatal(http.ListenAndServe(listen, mux))
}

func (s *server) handleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req ingestReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.UserID = strings.TrimSpace(req.UserID)
	req.Message = strings.TrimSpace(req.Message)
	if req.UserID == "" || req.Message == "" {
		http.Error(w, "user_id and message required", http.StatusBadRequest)
		return
	}
	if req.Role == "" {
		req.Role = "user"
	}

	start := time.Now()
	fact, err := s.extractFact(r.Context(), req.Role, req.Message)
	if err != nil {
		http.Error(w, "bedrock extract: "+err.Error(), http.StatusBadGateway)
		return
	}

	key := "mem:" + req.UserID
	stored := fact != "" && !strings.EqualFold(fact, "NONE")
	if stored {
		if err := s.rdb.RPush(r.Context(), key, fact).Err(); err != nil {
			http.Error(w, "redis: "+err.Error(), http.StatusBadGateway)
			return
		}
	}

	writeJSON(w, ingestResp{
		UserID: req.UserID,
		Fact:   fact,
		Key:    key,
		Stored: stored,
		Ms:     time.Since(start).Milliseconds(),
	})
}

func (s *server) handleContext(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	userID := strings.TrimSpace(r.URL.Query().Get("user_id"))
	query := strings.TrimSpace(r.URL.Query().Get("query"))
	if userID == "" || query == "" {
		http.Error(w, "user_id and query required", http.StatusBadRequest)
		return
	}
	k := 3
	if v := r.URL.Query().Get("k"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			k = n
		}
	}

	start := time.Now()
	key := "mem:" + userID
	all, err := s.rdb.LRange(r.Context(), key, 0, -1).Result()
	if err != nil {
		http.Error(w, "redis: "+err.Error(), http.StatusBadGateway)
		return
	}

	top := rankFacts(all, query, k)
	writeJSON(w, contextResp{
		UserID:  userID,
		Query:   query,
		Facts:   top,
		Context: strings.Join(top, "\n"),
		Total:   len(all),
		Ms:      time.Since(start).Milliseconds(),
	})
}

// extractFact asks Bedrock for a single compressed memory string.
func (s *server) extractFact(ctx context.Context, role, message string) (string, error) {
	user := fmt.Sprintf("role=%s\nmessage=%s", role, message)
	body, err := json.Marshal(map[string]any{
		"anthropic_version": "bedrock-2023-05-31",
		"max_tokens":        120,
		"temperature":       0,
		"system":            extractSystem,
		"messages": []map[string]string{
			{"role": "user", "content": user},
		},
	})
	if err != nil {
		return "", err
	}
	out, err := s.br.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(s.model),
		ContentType: aws.String("application/json"),
		Accept:      aws.String("application/json"),
		Body:        body,
	})
	if err != nil {
		return "", err
	}
	var resp struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out.Body, &resp); err != nil {
		return "", err
	}
	if len(resp.Content) == 0 {
		return "", fmt.Errorf("empty bedrock response")
	}
	return strings.TrimSpace(resp.Content[0].Text), nil
}

// rankFacts scores each stored fact by overlapping query tokens (case-insensitive).
// Ties keep Redis insertion order. Returns up to k facts with score > 0; if none
// match, returns the most recent min(k, len) facts so context is never empty.
func rankFacts(facts []string, query string, k int) []string {
	qTokens := tokens(query)
	type scored struct {
		fact  string
		score int
		idx   int
	}
	var ranked []scored
	for i, f := range facts {
		ft := tokens(f)
		score := 0
		for t := range qTokens {
			if ft[t] {
				score++
			}
		}
		ranked = append(ranked, scored{f, score, i})
	}
	// Stable-ish: higher score first, then more recent (higher idx).
	for i := 0; i < len(ranked); i++ {
		for j := i + 1; j < len(ranked); j++ {
			if ranked[j].score > ranked[i].score ||
				(ranked[j].score == ranked[i].score && ranked[j].idx > ranked[i].idx) {
				ranked[i], ranked[j] = ranked[j], ranked[i]
			}
		}
	}
	var out []string
	for _, s := range ranked {
		if s.score > 0 {
			out = append(out, s.fact)
			if len(out) == k {
				return out
			}
		}
	}
	if len(out) > 0 {
		return out
	}
	// No keyword hits — fall back to most recent facts.
	if k > len(facts) {
		k = len(facts)
	}
	start := len(facts) - k
	if start < 0 {
		start = 0
	}
	return append([]string(nil), facts[start:]...)
}

func tokens(s string) map[string]bool {
	out := map[string]bool{}
	var b strings.Builder
	flush := func() {
		if b.Len() == 0 {
			return
		}
		w := strings.ToLower(b.String())
		b.Reset()
		if len(w) < 3 { // skip tiny stop-ish tokens
			return
		}
		out[w] = true
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
