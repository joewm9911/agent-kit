package agent

import (
	_ "github.com/joewm9911/agent-kit/impl/session/inmemory" // 注册 inmemory 后端

	"github.com/joewm9911/agent-kit/session"
)

// inmemSession 经工厂拿一个进程内会话存储(不直接构造具体后端)。
func inmemSession(window int) session.Store {
	s, _ := session.New("inmemory", nil, window)
	return s
}
