// observe.go:可观测性装配。eino 全局回调是进程级切面,"本进程装过什么"
// 的账本由装配层(本文件)持有——observe 包只出纯构造,不持有状态。
// 幂等键取配置值本身(logger 身份 / 轨迹 path):同一配置重复装配(多次
// Build、副本重启测试)只装一份;不同配置各装一份、各收全量事件(全局
// 切面的语义即如此,按 app 过滤要走 eino per-invocation callback,另议)。
package config

import (
	"log/slog"
	"sync"

	"github.com/cloudwego/eino/callbacks"

	"github.com/joewm9911/agent-kit/observe"
)

var (
	obsMu         sync.Mutex
	logInstalled  = map[*slog.Logger]bool{}
	trajInstalled = map[string]bool{}
)

// installObservability 按配置装观测切面(进程级幂等,见文件头)。
func installObservability(oc ObservabilityConfig, logger *slog.Logger) error {
	obsMu.Lock()
	defer obsMu.Unlock()
	if oc.Log && !logInstalled[logger] {
		callbacks.AppendGlobalHandlers(observe.Handler(logger))
		logInstalled[logger] = true
	}
	if p := oc.TrajectoryPath; p != "" && !trajInstalled[p] {
		h, err := observe.Trajectory(p)
		if err != nil {
			return err
		}
		callbacks.AppendGlobalHandlers(h)
		trajInstalled[p] = true
	}
	return nil
}
