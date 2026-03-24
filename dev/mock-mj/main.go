package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"
)

var (
	port           int
	submitFailRate int
	execFailRate   int
	execDelayMs    int
)

func init() {
	flag.IntVar(&port, "port", 9091, "listen port")
	flag.IntVar(&submitFailRate, "submit-fail-rate", 20, "submit failure pct")
	flag.IntVar(&execFailRate, "exec-fail-rate", 30, "exec failure pct")
	flag.IntVar(&execDelayMs, "exec-delay", 5000, "task completion time ms")
}

type mjTask struct {
	mu         sync.RWMutex
	ID         string
	Action     string
	Prompt     string
	Status     string
	SubmitTime int64
	StartTime  int64
	FinishTime int64
	FailReason string
	Progress   string
	ImageURL   string
}

var (
	taskStore sync.Map
	idCounter int64
	idMu      sync.Mutex
)

func genID() string {
	idMu.Lock()
	idCounter++
	id := fmt.Sprintf("mock-%d-%d", time.Now().Unix(), idCounter)
	idMu.Unlock()
	return id
}

func main() {
	flag.Parse()
	mux := http.NewServeMux()

	actions := map[string]string{
		"/mj/submit/imagine":       "IMAGINE",
		"/mj/submit/blend":         "BLEND",
		"/mj/submit/describe":      "DESCRIBE",
		"/mj/submit/change":        "CHANGE",
		"/mj/submit/simple-change": "CHANGE",
		"/mj/submit/action":        "ACTION",
		"/mj/submit/shorten":       "SHORTEN",
		"/mj/submit/modal":         "MODAL",
		"/mj/submit/video":         "VIDEO",
		"/mj/submit/edits":         "EDITS",
	}
	for path, act := range actions {
		a := act
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			handleSubmit(w, r, a)
		})
	}

	mux.HandleFunc("/mj/task/list-by-condition", handleListByCondition)
	mux.HandleFunc("/mj/task/", handleTaskRoute)

	addr := fmt.Sprintf(":%d", port)
	log.Printf("Mock MJ on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func handleSubmit(w http.ResponseWriter, r *http.Request, action string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body map[string]interface{}
	json.NewDecoder(r.Body).Decode(&body)
	prompt, _ := body["prompt"].(string)
	if prompt == "" {
		prompt = "mock prompt"
	}

	if rand.Intn(100) < submitFailRate {
		code := 23
		if rand.Intn(2) == 0 {
			code = 24
		}
		log.Printf("[SUBMIT] %s REJECTED code=%d", action, code)
		writeJSON(w, map[string]interface{}{
			"code":        code,
			"description": "submit rejected",
			"properties":  map[string]interface{}{},
		})
		return
	}

	id := genID()
	now := time.Now().UnixMilli()
	t := &mjTask{
		ID: id, Action: action, Prompt: prompt,
		Status: "SUBMITTED", SubmitTime: now, StartTime: now, Progress: "0%",
	}
	taskStore.Store(id, t)
	log.Printf("[SUBMIT] %s OK id=%s", action, id)
	go runExec(t)
	writeJSON(w, map[string]interface{}{
		"code": 1, "description": "submit success", "result": id,
	})
}

func runExec(t *mjTask) {
	steps := 5
	d := time.Duration(execDelayMs/steps) * time.Millisecond
	for i := 1; i <= steps; i++ {
		time.Sleep(d)
		t.mu.Lock()
		t.Status = "IN_PROGRESS"
		t.Progress = fmt.Sprintf("%d%%", i*100/steps)
		t.mu.Unlock()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.FinishTime = time.Now().UnixMilli()
	if rand.Intn(100) < execFailRate {
		t.Status = "FAILURE"
		t.Progress = ""
		reasons := []string{"Banned prompt", "Job error", "Timeout", "Invalid param"}
		t.FailReason = reasons[rand.Intn(len(reasons))]
		log.Printf("[EXEC] %s FAILURE: %s", t.ID, t.FailReason)
	} else {
		t.Status = "SUCCESS"
		t.Progress = "100%"
		t.ImageURL = fmt.Sprintf("https://mock-cdn.example.com/%s.png", t.ID)
		log.Printf("[EXEC] %s SUCCESS", t.ID)
	}
}

func handleTaskRoute(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if strings.HasSuffix(path, "/fetch") {
		id := extractID(path, "/fetch")
		if id == "" {
			writeJSON(w, map[string]interface{}{"code": 4, "description": "task_no_found"})
			return
		}
		v, ok := taskStore.Load(id)
		if !ok {
			writeJSON(w, map[string]interface{}{"code": 4, "description": "task_no_found"})
			return
		}
		tk := v.(*mjTask)
		tk.mu.RLock()
		dto := buildDTO(tk)
		tk.mu.RUnlock()
		writeJSON(w, dto)
		return
	}
	if strings.HasSuffix(path, "/image-seed") {
		id := extractID(path, "/image-seed")
		v, ok := taskStore.Load(id)
		if !ok || id == "" {
			writeJSON(w, map[string]interface{}{"code": 4, "description": "task_no_found"})
			return
		}
		tk := v.(*mjTask)
		tk.mu.RLock()
		defer tk.mu.RUnlock()
		if tk.Status != "SUCCESS" {
			writeJSON(w, map[string]interface{}{"code": 4, "description": "not finished"})
			return
		}
		writeJSON(w, map[string]interface{}{
			"code": 1, "description": "success",
			"result": fmt.Sprintf("%d", rand.Int63n(999999999)),
		})
		return
	}
	http.NotFound(w, r)
}

func extractID(path, suffix string) string {
	pfx := "/mj/task/"
	if !strings.HasPrefix(path, pfx) || !strings.HasSuffix(path, suffix) {
		return ""
	}
	mid := path[len(pfx) : len(path)-len(suffix)]
	return strings.TrimSuffix(mid, "/")
}

func handleListByCondition(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		IDs []string `json:"ids"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	result := make([]map[string]interface{}, 0)
	for _, id := range req.IDs {
		v, ok := taskStore.Load(id)
		if !ok {
			continue
		}
		tk := v.(*mjTask)
		tk.mu.RLock()
		result = append(result, buildDTO(tk))
		tk.mu.RUnlock()
	}
	writeJSON(w, result)
}

func buildDTO(t *mjTask) map[string]interface{} {
	btns := []map[string]interface{}{}
	if t.Status == "SUCCESS" {
		btns = []map[string]interface{}{
			{"customId": "MJ::JOB::upsample::1", "label": "U1", "style": 2},
			{"customId": "MJ::JOB::upsample::2", "label": "U2", "style": 2},
			{"customId": "MJ::JOB::variation::1", "label": "V1", "style": 2},
			{"customId": "MJ::JOB::variation::2", "label": "V2", "style": 2},
		}
	}
	return map[string]interface{}{
		"id": t.ID, "action": t.Action, "prompt": t.Prompt,
		"promptEn": t.Prompt, "status": t.Status,
		"submitTime": t.SubmitTime, "startTime": t.StartTime,
		"finishTime": t.FinishTime, "failReason": t.FailReason,
		"progress": t.Progress, "imageUrl": t.ImageURL,
		"buttons": btns, "properties": map[string]interface{}{},
		"description": "",
	}
}

func writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}
