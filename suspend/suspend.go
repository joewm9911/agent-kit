// Package suspend 实现挂起/恢复的持久化:ask_user 与审批等待不再靠
// 进程内 goroutine 阻塞——飞书审批常常跨小时甚至隔天,占着 goroutine
// 等、进程一重启全丢,撑不住真实使用。
//
// 机制是"卸载重放"(durable execution lite):
//
//   - 挂起:交互点(Ask/Approve)查不到已记录的答案时,持久化待答
//     记录并返回 ErrSuspended,整轮调用栈退干净,进程不持有任何状态;
//   - 恢复:用户的答案(可能在进程重启之后)写入交互日志,原输入
//     重跑该轮——交互点按确定性键命中日志直接返回答案,越过挂起点;
//   - 幂等:mutating 工具的执行结果随轮记入效果日志,重放时命中即
//     返回记录结果、不二次执行(只读工具与模型调用照常重跑,安全)。
//
// 重放路径若与首次运行分叉(模型换了措辞/换了路径),交互键不再命中,
// 退化为重新提问——多问一次,但不会答非所问,失败模式是安全的。
package suspend

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/joewm9911/agent-kit/runctx"
)

// NewTurnID 生成一轮的唯一标识(时间 + 随机),恢复的轮次沿用首跑的 ID。
func NewTurnID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}

// ErrSuspended 表示运行在交互点挂起,等待用户答复后重放。
type ErrSuspended struct {
	InteractionID string // 待答交互的确定性键
	Question      string // 已送达用户的问题
}

func (e *ErrSuspended) Error() string {
	return "run suspended, waiting for user reply (interaction " + e.InteractionID + ")"
}

// Store 是挂起状态的持久化后端。kind 划分记录类型(ask/effect/turn),
// key 在 kind 内唯一。实现必须可跨进程重启读回。
type Store interface {
	Put(kind, key string, value []byte) error
	Get(kind, key string) ([]byte, bool, error)
	Delete(kind, key string) error
	// List 返回某 kind 下全部记录(key → value),用于按前缀清理。
	List(kind string) (map[string][]byte, error)
}

const (
	kindAsk    = "ask"    // 交互日志:问题与答案
	kindEffect = "effect" // 效果日志:mutating 工具的执行结果
	kindTurn   = "turn"   // 挂起中的轮次:会话 → {轮次ID, 原输入, 待答交互}
)

// PendingTurn 是一条挂起中的轮次记录。
type PendingTurn struct {
	TurnID    string `json:"turn_id"`
	Input     string `json:"input"`
	WaitingID string `json:"waiting_id"` // 挂起时等待的交互键
}

type askRecord struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
	Done     bool   `json:"done"`
}

// Journal 是一轮运行的挂起日志视图,由分发层创建并装入 ctx。
type Journal struct {
	store Store
	turn  string
}

// NewJournal 创建某一轮(turnID 恢复时必须与首跑一致)的日志视图。
func NewJournal(store Store, turnID string) *Journal {
	return &Journal{store: store, turn: turnID}
}

// TurnID 返回本轮标识。
func (j *Journal) TurnID() string { return j.turn }

// interactionID 生成交互的确定性键:同一轮里同一个问题重放时命中同一条记录。
func (j *Journal) interactionID(question string) string {
	sum := sha256.Sum256([]byte(j.turn + "\x00" + question))
	return j.turn + "-" + hex.EncodeToString(sum[:8])
}

// answer 查交互日志。
func (j *Journal) answer(id string) (string, bool, error) {
	raw, ok, err := j.store.Get(kindAsk, id)
	if err != nil || !ok {
		return "", false, err
	}
	var rec askRecord
	if json.Unmarshal(raw, &rec) != nil || !rec.Done {
		return "", false, nil
	}
	return rec.Answer, true, nil
}

// recordPending 持久化一条待答交互。
func (j *Journal) recordPending(id, question string) error {
	raw, _ := json.Marshal(askRecord{Question: question})
	return j.store.Put(kindAsk, id, raw)
}

// AnswerPending 写入用户答案(恢复入口,进程重启后同样可用)。
func AnswerPending(store Store, interactionID, answer string) error {
	raw, ok, err := store.Get(kindAsk, interactionID)
	if err != nil {
		return err
	}
	var rec askRecord
	if ok {
		_ = json.Unmarshal(raw, &rec)
	}
	rec.Answer, rec.Done = answer, true
	out, _ := json.Marshal(rec)
	return store.Put(kindAsk, interactionID, out)
}

// effectKey 生成效果键:轮次 + 能力 + 参数哈希。并行分支下不依赖
// 执行顺序,重放确定命中;代价是同轮同参的重复调用会去重(极少数
// "同一参数发两次"的场景不适用,应拆参数)。
func (j *Journal) effectKey(capKey, argsJSON string) string {
	sum := sha256.Sum256([]byte(capKey + "\x00" + argsJSON))
	return j.turn + "-" + hex.EncodeToString(sum[:8])
}

// Effect 查效果日志。
func (j *Journal) Effect(capKey, argsJSON string) (string, bool) {
	raw, ok, err := j.store.Get(kindEffect, j.effectKey(capKey, argsJSON))
	if err != nil || !ok {
		return "", false
	}
	return string(raw), true
}

// SaveEffect 记录一次 mutating 执行的结果。
func (j *Journal) SaveEffect(capKey, argsJSON, result string) {
	_ = j.store.Put(kindEffect, j.effectKey(capKey, argsJSON), []byte(result))
}

// CompleteTurn 在一轮成功结束后清理该轮的全部日志。
func (j *Journal) CompleteTurn() {
	for _, kind := range []string{kindAsk, kindEffect} {
		if all, err := j.store.List(kind); err == nil {
			for k := range all {
				if strings.HasPrefix(k, j.turn+"-") {
					_ = j.store.Delete(kind, k)
				}
			}
		}
	}
}

// SavePendingTurn 持久化挂起中的轮次(同会话同时只有一条)。
func SavePendingTurn(store Store, sessionKey string, rec PendingTurn) error {
	raw, _ := json.Marshal(rec)
	return store.Put(kindTurn, sessionKey, raw)
}

// TakePendingTurn 取出并删除会话的挂起轮次(答案到达时的恢复入口)。
func TakePendingTurn(store Store, sessionKey string) (PendingTurn, bool, error) {
	raw, ok, err := store.Get(kindTurn, sessionKey)
	if err != nil || !ok {
		return PendingTurn{}, false, err
	}
	var rec PendingTurn
	if err := json.Unmarshal(raw, &rec); err != nil {
		return PendingTurn{}, false, err
	}
	return rec, true, store.Delete(kindTurn, sessionKey)
}

type keyJournal struct{}

// WithJournal 把挂起日志装入 ctx,对下游交互与效果闸门生效。
func WithJournal(ctx context.Context, j *Journal) context.Context {
	if j == nil {
		return ctx
	}
	return context.WithValue(ctx, keyJournal{}, j)
}

// JournalFrom 取 ctx 中的挂起日志,可能为 nil。
func JournalFrom(ctx context.Context) *Journal {
	j, _ := ctx.Value(keyJournal{}).(*Journal)
	return j
}

// Notify 把挂起的问题送达用户(发进 IM 会话等),由分发层提供。
type Notify func(ctx context.Context, question string) error

// Interactor 返回可挂起的交互通道:Ask/Approve 不阻塞——命中日志即
// 返回答案(重放),未命中则送出问题、记录待答并以 ErrSuspended 退栈。
func Interactor(j *Journal, notify Notify) runctx.Interactor {
	return &suspendingInteractor{j: j, notify: notify}
}

type suspendingInteractor struct {
	j      *Journal
	notify Notify
	mu     sync.Mutex
}

func (s *suspendingInteractor) Ask(ctx context.Context, question string) (string, error) {
	return s.resolve(ctx, question)
}

func (s *suspendingInteractor) Approve(ctx context.Context, req runctx.ApprovalRequest) (bool, error) {
	q := fmt.Sprintf("需要你批准一个操作:\n%s\n参数:%s\n回复「同意」执行,回复其他内容取消。", req.Description, req.Arguments)
	ans, err := s.resolve(ctx, q)
	if err != nil {
		return false, err
	}
	ans = strings.TrimSpace(ans)
	return ans == "同意" || strings.EqualFold(ans, "y") || strings.EqualFold(ans, "yes"), nil
}

func (s *suspendingInteractor) resolve(ctx context.Context, question string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.j.interactionID(question)
	if ans, ok, err := s.j.answer(id); err == nil && ok {
		return ans, nil // 重放:越过挂起点
	}
	if err := s.j.recordPending(id, question); err != nil {
		return "", err
	}
	if err := s.notify(ctx, question); err != nil {
		return "", err
	}
	return "", &ErrSuspended{InteractionID: id, Question: question}
}
