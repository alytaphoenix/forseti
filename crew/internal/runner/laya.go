package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"forseti/crew/internal/schema"
)

// LayaClient asks the local Laya decision endpoint (S18: POST /v1/systemone
// {state, questions} → {model, answers{choice|score|noul, probabilities,
// confidence}, usage, routing}). The endpoint is a user-invoked process —
// a dead endpoint is a fail-safe skip, never a hang.
type LayaClient struct {
	BaseURL string
	HTTP    *http.Client
}

func NewLayaClient() *LayaClient {
	url := os.Getenv("FORSETI_LAYA_URL")
	if url == "" {
		url = "http://127.0.0.1:8751"
	}
	return &LayaClient{BaseURL: strings.TrimRight(url, "/"), HTTP: &http.Client{Timeout: 15 * time.Second}}
}

// layaChoice is one option in a choice question: the target node name and its
// edge's instructions (why this option would fit).
type layaChoice struct {
	Target       string // node name ("" = the implicit "other" escape)
	Instructions string
}

// Decide routes a bounded choice over targets: one endpoint call, the argmax
// wins, distributions + latency recorded via the returned record.
// Returns (chosen target, confidence, full record). The implicit "other"
// option maps to "" (no activation).
func (c *LayaClient) Decide(state string, choices []layaChoice) (string, float64, map[string]any, error) {
	// build the criteria table in crew order (deterministic)
	criteria := map[string]string{}
	names := make([]string, 0, len(choices)+1)
	for _, ch := range choices {
		criteria[ch.Target] = ch.Instructions
		names = append(names, ch.Target)
	}
	criteria["other"] = "none of the options fit this output"
	names = append(names, "other")

	q := map[string]any{
		"type":         "choice",
		"instructions": "Which option best fits this output? Choose 'other' if none fit.",
		"criteria":     criteria,
	}
	body, err := json.Marshal(map[string]any{
		"state":     map[string]any{"output": state},
		"questions": map[string]any{"route": q},
	})
	if err != nil {
		return "", 0, nil, err
	}
	t0 := time.Now()
	resp, err := c.HTTP.Post(c.BaseURL+"/v1/systemone", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", 0, nil, fmt.Errorf("laya endpoint unreachable (%s): %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", 0, nil, fmt.Errorf("laya endpoint %d", resp.StatusCode)
	}
	var out struct {
		Model   string `json:"model"`
		Answers map[string]struct {
			Choice           string         `json:"choice"`
			Probabilities    map[string]any `json:"probabilities"`
			Confidence       float64        `json:"confidence"`
			AnswerConfidence float64        `json:"answer_confidence"`
		} `json:"answers"`
		Routing map[string]any `json:"routing"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", 0, nil, fmt.Errorf("laya decode: %w", err)
	}
	ans, ok := out.Answers["route"]
	if !ok {
		return "", 0, nil, fmt.Errorf("laya answer missing 'route' key")
	}
	chosen := ""
	if ans.Choice == "other" {
		chosen = "" // explicit abstention to no-activation
	} else {
		chosen = ans.Choice
	}
	rec := map[string]any{
		"model":             out.Model,
		"checkpoint":        out.Routing["model"],
		"chosen":            chosen,
		"confidence":        ans.Confidence,       // calibrated (temperature) — the gate signal
		"answer_confidence": ans.AnswerConfidence, // top-option probability share
		"probabilities":     ans.Probabilities,
		"latency_ms":        time.Since(t0).Milliseconds(),
	}
	return chosen, ans.Confidence, rec, nil
}

// sortKeys for deterministic event info rendering.
func sortKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// MemoryClient is the shared-memory consumer (P10-2): recall-only from the
// runner (writes come from agents' tools + crew check nodes).
type MemoryClient struct {
	BaseURL string
	HTTP    *http.Client
}

func NewMemoryClient() *MemoryClient {
	url := os.Getenv("FORSETI_MEMORY_URL")
	if url == "" {
		url = "http://127.0.0.1:8752"
	}
	return &MemoryClient{BaseURL: strings.TrimRight(url, "/"), HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// Recall returns rows (map form: id/ts/agent/text/score) or an error when the
// service is unreachable. crew (P14) widens visibility to the crew namespace
// (empty = personal + shared only).
func (c *MemoryClient) Recall(query, agent string, k int, crew string) ([]map[string]any, error) {
	if k <= 0 {
		k = 3
	}
	payload := map[string]any{"query": query, "agent": agent, "k": k}
	if crew != "" {
		payload["crew"] = crew
	}
	body, _ := json.Marshal(payload)
	resp, err := c.HTTP.Post(c.BaseURL+"/recall", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("memory endpoint unreachable (%s): %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("memory endpoint %d", resp.StatusCode)
	}
	var rows []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// Write posts a node's output back to the crew memory namespace (P14 auto-
// write). The service stores the row under crew-<crew>. Advisory: the runner
// fires this off the hot path; errors are logged, never fatal to the run.
func (c *MemoryClient) Write(text, crew, run, source string, tags []string) (int, error) {
	if len(text) > 4000 {
		text = text[:4000]
	}
	body, _ := json.Marshal(map[string]any{
		"agent": "crew-" + crew, "crew": crew, "text": text,
		"run": run, "source": source, "tags": tags,
	})
	resp, err := c.HTTP.Post(c.BaseURL+"/write", "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("memory endpoint unreachable (%s): %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("memory write %d", resp.StatusCode)
	}
	var out struct {
		ID int `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// edgeIsLaya is a schema helper alias (kept here so runner code reads plain).
func edgeIsLaya(e schema.Edge) bool { return e.IsLaya() }
