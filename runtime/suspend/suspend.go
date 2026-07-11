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
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/joewm9911/agent-kit/core/runctx"
	"github.com/joewm9911/agent-kit/protocol/store"
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

// TurnTerminal 标记本错误为"轮次终止级":必须穿透工具错误转结果的
// 兜底 middleware(engine 侧按此接口放行),否则挂起信号会被吞掉。
func (e *ErrSuspended) TurnTerminal() {}

func (e *ErrSuspended) Error() string {
	return "run suspended, waiting for user reply (interaction " + e.InteractionID + ")"
}

// 持久化收敛到 store.KV 原语:kind(记录类型)并入键前缀,任何 KV 后端
// (file/redis/自定义)都可承载挂起状态——多副本部署时挂起与恢复可以落在
// 不同副本。跨进程重启可恢复要求后端持久(file/redis);inmemory 仅测试用。
const (
	kindAsk    = "ask"    // 交互日志:问题与答案
	kindEffect = "effect" // 效果日志:mutating 工具的执行结果
	kindTurn   = "turn"   // 挂起中的轮次:会话 → {轮次ID, 原输入, 待答交互}

	ksep = "\x1f" // kind 与 key 的分隔符(不可见字符,键内容任意)
)

func kkey(kind, key string) string { return kind + ksep + key }

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
// 持有注入的 KV 后端,不感知具体实现。
type Journal struct {
	kv   store.KV
	turn string
}

// NewJournal 创建某一轮(turnID 恢复时必须与首跑一致)的日志视图。
func NewJournal(kv store.KV, turnID string) *Journal {
	return &Journal{kv: kv, turn: turnID}
}

// TurnID 返回本轮标识。
func (j *Journal) TurnID() string { return j.turn }

// interactionID 生成交互的确定性键:同一轮里同一个问题重放时命中同一条记录。
func (j *Journal) interactionID(question string) string {
	sum := sha256.Sum256([]byte(j.turn + "\x00" + question))
	return j.turn + "-" + hex.EncodeToString(sum[:8])
}

// answer 查交互日志。
func (j *Journal) answer(ctx context.Context, id string) (string, bool, error) {
	raw, ok, err := j.kv.Get(ctx, kkey(kindAsk, id))
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
func (j *Journal) recordPending(ctx context.Context, id, question string) error {
	raw, _ := json.Marshal(askRecord{Question: question})
	return put(ctx, j.kv, kkey(kindAsk, id), raw)
}

// put 是"直接写入"的 KV 便捷封装(挂起日志无并发改写,不需读改写)。
func put(ctx context.Context, kv store.KV, key string, value []byte) error {
	return kv.Update(ctx, key, func([]byte, bool) ([]byte, error) {
		return value, nil
	}, 0)
}

// AnswerPending 写入用户答案(恢复入口,进程重启后同样可用)。
func AnswerPending(ctx context.Context, kv store.KV, interactionID, answer string) error {
	return kv.Update(ctx, kkey(kindAsk, interactionID), func(old []byte, ok bool) ([]byte, error) {
		var rec askRecord
		if ok {
			_ = json.Unmarshal(old, &rec)
		}
		rec.Answer, rec.Done = answer, true
		return json.Marshal(rec)
	}, 0)
}

// effectKey 生成效果键:轮次 + 能力 + 参数哈希。并行分支下不依赖
// 执行顺序,重放确定命中;代价是同轮同参的重复调用会去重(极少数
// "同一参数发两次"的场景不适用,应拆参数)。
func (j *Journal) effectKey(capKey, argsJSON string) string {
	sum := sha256.Sum256([]byte(capKey + "\x00" + argsJSON))
	return j.turn + "-" + hex.EncodeToString(sum[:8])
}

// effectInFlight 是效果日志的"已开始执行"哨兵值:执行前先落此标记,执行后
// 覆盖为真实结果。重放时若命中哨兵 = 上次执行已发起但结果未记录(结果写失败
// 或执行中崩溃)——绝不能自动重放,以确定性文本收口交人工确认。
const effectInFlight = "\x00effect-in-flight"

// Effect 查效果日志。命中哨兵时以"不可自动重试"的说明文本返回(ok=true,
// 挡住重放路径的再执行)。
func (j *Journal) Effect(ctx context.Context, capKey, argsJSON string) (string, bool) {
	raw, ok, err := j.kv.Get(ctx, kkey(kindEffect, j.effectKey(capKey, argsJSON)))
	if err != nil || !ok {
		return "", false
	}
	if string(raw) == effectInFlight {
		return "该操作在挂起前已实际执行,但结果未能记录(记录写入失败或执行中断)。为避免重复副作用,重放不再自动执行;请人工核对该操作的实际结果后继续。", true
	}
	return string(raw), true
}

// BeginEffect 在执行 mutating 能力**之前**落"已开始"标记:此写失败则拒绝
// 执行(副作用尚未发生,失败安全)——这是效果日志幂等保证的前置台账。
func (j *Journal) BeginEffect(ctx context.Context, capKey, argsJSON string) error {
	return put(ctx, j.kv, kkey(kindEffect, j.effectKey(capKey, argsJSON)), []byte(effectInFlight))
}

// SaveEffect 把"已开始"标记覆盖为真实结果。写失败不能吞:标记会留在台账里,
// 后续重放命中哨兵、拒绝自动再执行(见 Effect)——宁可要求人工确认,也不
// 二次执行已审批的 mutating 操作。此处只能留痕。
func (j *Journal) SaveEffect(ctx context.Context, capKey, argsJSON, result string) {
	if err := put(ctx, j.kv, kkey(kindEffect, j.effectKey(capKey, argsJSON)), []byte(result)); err != nil {
		slog.Warn("suspend: effect result write failed; in-flight marker stays, replay will require manual confirmation",
			"cap", capKey, "err", err)
	}
}

// CompleteTurn 在一轮成功结束后清理该轮的全部日志。
func (j *Journal) CompleteTurn(ctx context.Context) {
	for _, kind := range []string{kindAsk, kindEffect} {
		if keys, err := j.kv.Scan(ctx, kkey(kind, j.turn+"-")); err == nil {
			for _, k := range keys {
				_ = j.kv.Delete(ctx, k)
			}
		}
	}
}

// SavePendingTurn 持久化挂起中的轮次(同会话同时只有一条)。
func SavePendingTurn(ctx context.Context, kv store.KV, sessionKey string, rec PendingTurn) error {
	raw, _ := json.Marshal(rec)
	return put(ctx, kv, kkey(kindTurn, sessionKey), raw)
}

// errNoPending 是原子认领的"无挂起记录"哨兵(仅包内)。
var errNoPending = errors.New("no pending turn")

// TakePendingTurn 取出并删除会话的挂起轮次(答案到达时的恢复入口)。
// 认领经后端原子读改写完成(读到即删,mutate 返回 nil = 删除):Get 后再
// Delete 的两步认领在多副本下会让同一挂起轮被两个副本各认领一次、双重放
// ——重放里的 mutating 操作随之竞态。损坏的记录也在同一原子操作里消费掉
// (否则它永远留在键上,该会话每条消息都撞同一错误,砖死)。
func TakePendingTurn(ctx context.Context, kv store.KV, sessionKey string) (PendingTurn, bool, error) {
	var rec PendingTurn
	var corrupt error
	err := kv.Update(ctx, kkey(kindTurn, sessionKey), func(old []byte, ok bool) ([]byte, error) {
		if !ok {
			return nil, errNoPending
		}
		if uerr := json.Unmarshal(old, &rec); uerr != nil {
			corrupt = uerr // 消费掉损坏记录(返回 nil 删除),错误带出
		}
		return nil, nil // 删除 = 认领成功
	}, 0)
	if errors.Is(err, errNoPending) {
		return PendingTurn{}, false, nil
	}
	if err != nil {
		return PendingTurn{}, false, err
	}
	if corrupt != nil {
		slog.Warn("suspend: corrupt pending-turn record consumed", "session", sessionKey, "err", corrupt)
		return PendingTurn{}, false, fmt.Errorf("pending turn record corrupt (consumed): %w", corrupt)
	}
	return rec, true, nil
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
	ans, err := s.resolve(ctx, fmt.Sprintf(DefaultApprovalPrompt, req.Description, req.Arguments))
	if err != nil {
		return false, err
	}
	return IsAffirmative(ans), nil
}

// DefaultApprovalPrompt formats the approval question sent to the user;
// the two %s are the operation description and its arguments. Shared by the
// suspend-mode and in-process HITL paths so the wording stays identical.
const DefaultApprovalPrompt = "Approval required for an operation:\n%s\nArguments: %s\nReply \"yes\" to proceed, anything else to cancel."

// IsAffirmative reports whether a user's reply approves the pending
// operation. It accepts common affirmatives in English and Chinese so a
// multilingual deployment works without extra configuration.
func IsAffirmative(reply string) bool {
	switch strings.ToLower(strings.TrimSpace(reply)) {
	case "y", "yes", "ok", "approve":
		return true
	}
	switch strings.TrimSpace(reply) {
	case "同意", "是", "批准", "好":
		return true
	}
	return false
}

func (s *suspendingInteractor) resolve(ctx context.Context, question string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.j.interactionID(question)
	ans, ok, err := s.j.answer(ctx, id)
	if err != nil {
		// 读抖动 ≠ 无答案:此处若照常落 recordPending,会把用户已写入的
		// Answer/Done 盲目覆盖成空问题记录——把一次可重试的读失败变成
		// 破坏性的答案丢失 + 重复提问。上抛让本轮失败重试。
		return "", fmt.Errorf("suspend journal read: %w", err)
	}
	if ok {
		return ans, nil // 重放:越过挂起点
	}
	if err := s.j.recordPending(ctx, id, question); err != nil {
		return "", err
	}
	if err := s.notify(ctx, question); err != nil {
		return "", err
	}
	return "", &ErrSuspended{InteractionID: id, Question: question}
}
