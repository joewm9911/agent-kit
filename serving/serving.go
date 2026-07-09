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

	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/channel"
	"github.com/joewm9911/agent-kit/protocol/store"
	"github.com/joewm9911/agent-kit/runtime/suspend"
)

// Server 是 Gateway 实例。
type Server struct {
	addr   string
	mux    *http.ServeMux
	agents map[string]Runnable
	logger *slog.Logger

	// suspendKV 非 nil 时 /messages 的 JSON 路径启用持久化挂起(与
	// dispatcher 的 IM 路径同一机制、同一后端):ask_user/审批不占请求
	// 等待,响应 waiting 态;同会话的下一个请求即答案。SSE 流式路径
	// 不参与(问句无法安放在增量事件流的语义里)。
	suspendKV store.KV
}

// New 创建 Gateway 并注册 agent 路由。
func New(addr string, agents []Runnable, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{addr: addr, mux: http.NewServeMux(), agents: map[string]Runnable{}, logger: logger}
	for _, a := range agents {
		s.agents[a.Name()] = a
	}
	s.routes()
	return s
}

// Mux 暴露路由器,channel 的 webhook 注册在此。
func (s *Server) Mux() *http.ServeMux { return s.mux }

// EnableSuspend 启用 /messages 的持久化挂起,后端与 IM 通道共用。
func (s *Server) EnableSuspend(kv store.KV) {
	s.suspendKV = kv
}

// AttachChannel 把一个 channel 绑定到 agent 并挂载其 webhook。
func (s *Server) AttachChannel(ctx context.Context, ch channel.Channel, d *Dispatcher, b Binding) error {
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
	// 运行控制:中断/插话不经会话串行队列,对进行中的运行即时生效
	s.mux.HandleFunc("POST /agents/{name}/control", s.handleControl)
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
		User    string `json:"user"` // 终端用户身份,长期记忆用户级隔离据此施加
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Input == "" {
		http.Error(w, "bad request: need {session, input}", http.StatusBadRequest)
		return
	}
	if req.Session == "" {
		req.Session = "http-" + suspend.NewTurnID() // 时间+随机,并发不碰撞、可读可排序
	}
	ctx := runctx.WithUser(r.Context(), req.User)
	ctx = applyContextHooks(ctx, InboundInfo{Channel: "http", User: req.User, Session: req.Session})

	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.stream(w, ctx, a, req.Session, req.Input)
		return
	}
	if s.suspendKV != nil {
		s.suspendableMessage(w, ctx, a, req.Session, req.Input)
		return
	}
	answer, err := a.Run(ctx, req.Session, req.Input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"session": req.Session, "status": "done", "answer": answer})
}

// suspendableMessage 以挂起模式执行一轮:编排(认领答案/重放/挂起收口)
// 与 dispatcher 的 IM 路径共用 suspendturn.go 的同一份机制,这里只做
// HTTP 的传输策略——问句不独立投递(notify 置空),挂起后落响应体
// {status: "waiting", question};正常完成 {status: "done", answer}。
//
// 同会话并发请求没有串行保护(dispatcher 的会话队列是 IM 语义,不在
// HTTP 路径重造),调用方须按会话串行调用。
func (s *Server) suspendableMessage(w http.ResponseWriter, ctx context.Context, a Runnable, session, input string) {
	turnInput, turnID := input, ""
	if rec, resumed, err := resumePending(ctx, s.suspendKV, session, input); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if resumed {
		turnInput, turnID = rec.Input, rec.TurnID
	}
	ctx, turn := beginSuspendTurn(ctx, s.suspendKV, turnID, nil)

	answer, runErr := a.Run(ctx, session, turnInput)
	question, suspended, err := turn.finish(ctx, session, turnInput, runErr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if suspended {
		writeJSON(w, map[string]string{"session": session, "status": "waiting", "question": question})
		return
	}
	writeJSON(w, map[string]string{"session": session, "status": "done", "answer": answer})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// handleControl 处理运行控制:{session, action: interrupt|steer, message}。
// 与 IM 会话里的「停止」「插话:」文本指令是同一 Agent.Interrupt/Steer
// 机制的两个传输入口,语义完全一致(不经会话队列,对进行中的运行即时生效)。
func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	a, ok := s.agents[r.PathValue("name")]
	if !ok {
		http.Error(w, "unknown agent", http.StatusNotFound)
		return
	}
	var req struct {
		Session string `json:"session"`
		Action  string `json:"action"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Session == "" {
		http.Error(w, "bad request: need {session, action}", http.StatusBadRequest)
		return
	}
	switch req.Action {
	case "interrupt":
		a.Interrupt(req.Session)
	case "steer":
		if req.Message == "" {
			http.Error(w, "steer needs {message}", http.StatusBadRequest)
			return
		}
		a.Steer(req.Session, req.Message)
	default:
		http.Error(w, "bad action: want interrupt|steer", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) stream(w http.ResponseWriter, ctx context.Context, a Runnable, session, input string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	sr, err := a.Stream(ctx, session, input)
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
	session := "a2a-" + suspend.NewTurnID()
	ctx := applyContextHooks(r.Context(), InboundInfo{Channel: "a2a", Session: session})
	result, err := a.Run(ctx, session, req.Task)
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"result": result})
}
