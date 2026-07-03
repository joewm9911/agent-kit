// Package serving 是 Gateway:一个进程同时暴露三张脸——
//   - 对人/前端:POST /agents/{name}/messages(JSON 或 SSE 流式);
//   - 对其他 agent:A2A 协议(GET /a2a/agents、POST /a2a/agents/{name}/tasks),
//     与 provider/a2a 的消费端同一协议,agentkit 部署之间天然互通;
//   - 对 IM:承载各 channel 的 webhook 路由。
package serving

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cloverzhang/agent-kit/agent"
	"github.com/cloverzhang/agent-kit/channel"
)

// Server 是 Gateway 实例。
type Server struct {
	addr   string
	mux    *http.ServeMux
	agents map[string]*agent.Agent
	logger *slog.Logger
}

// New 创建 Gateway 并注册 agent 路由。
func New(addr string, agents []*agent.Agent, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{addr: addr, mux: http.NewServeMux(), agents: map[string]*agent.Agent{}, logger: logger}
	for _, a := range agents {
		s.agents[a.Name()] = a
	}
	s.routes()
	return s
}

// Mux 暴露路由器,channel 的 webhook 注册在此。
func (s *Server) Mux() *http.ServeMux { return s.mux }

// AttachChannel 把一个 channel 绑定到 agent 并挂载其 webhook。
func (s *Server) AttachChannel(ctx context.Context, ch channel.Channel, d *channel.Dispatcher, b channel.Binding) error {
	return ch.Start(ctx, s.mux, d.Handler(b))
}

// Run 启动服务,阻塞直到 ctx 取消。
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{Addr: s.addr, Handler: s.mux, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	s.logger.Info("gateway listening", slog.String("addr", s.addr))
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

func (s *Server) routes() {
	// 对人/前端
	s.mux.HandleFunc("POST /agents/{name}/messages", s.handleMessage)
	// A2A 供给面
	s.mux.HandleFunc("GET /a2a/agents", s.handleA2AList)
	s.mux.HandleFunc("POST /a2a/agents/{name}/tasks", s.handleA2ATask)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	a, ok := s.agents[r.PathValue("name")]
	if !ok {
		http.Error(w, "unknown agent", http.StatusNotFound)
		return
	}
	var req struct {
		Session string `json:"session"`
		Input   string `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Input == "" {
		http.Error(w, "bad request: need {session, input}", http.StatusBadRequest)
		return
	}
	if req.Session == "" {
		req.Session = fmt.Sprintf("http-%d", time.Now().UnixNano())
	}

	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.stream(w, r, a, req.Session, req.Input)
		return
	}
	answer, err := a.Run(r.Context(), req.Session, req.Input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"session": req.Session, "answer": answer})
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request, a *agent.Agent, session, input string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	sr, err := a.Stream(r.Context(), session, input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer sr.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	for {
		chunk, e := sr.Recv()
		if e != nil {
			break
		}
		if chunk.Content == "" {
			continue
		}
		b, _ := json.Marshal(map[string]string{"delta": chunk.Content})
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (s *Server) handleA2AList(w http.ResponseWriter, _ *http.Request) {
	type info struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	out := make([]info, 0, len(s.agents))
	for _, a := range s.agents {
		out = append(out, info{Name: a.Name(), Description: a.Description()})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleA2ATask(w http.ResponseWriter, r *http.Request) {
	a, ok := s.agents[r.PathValue("name")]
	if !ok {
		http.Error(w, "unknown agent", http.StatusNotFound)
		return
	}
	var req struct {
		Task string `json:"task"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Task == "" {
		http.Error(w, "bad request: need {task}", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// A2A 任务无会话延续:每次独立会话,与子 agent 语义一致。
	session := fmt.Sprintf("a2a-%d", time.Now().UnixNano())
	result, err := a.Run(r.Context(), session, req.Task)
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"result": result})
}
