package loop

import "github.com/joewm9911/agent-kit/core/capability"

// Duration 是配置时长字段;定义已下沉基座 capability(见 capability/duration.go),
// 这里保留别名兼容既有 loop.Duration 引用。
type Duration = capability.Duration
