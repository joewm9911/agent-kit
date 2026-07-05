// Package std 空导入即拉起 agent-kit 的默认存储后端(session/memory 的
// inmemory·file·bigram),恢复开箱即用的 zero-config。生产按需再空导入
// impl/session/redis、impl/memory/redis 等外部后端。
//
//	import _ "github.com/joewm9911/agent-kit/std"
//
// (store.KV inmemory、prompt inline/file/http、vector 词法后端仍随各自协议
// 包常驻,无需在此拉起。)
package std

import (
	_ "github.com/joewm9911/agent-kit/impl/memory/inmemory"
	_ "github.com/joewm9911/agent-kit/impl/session/bigram"
	_ "github.com/joewm9911/agent-kit/impl/session/file"
	_ "github.com/joewm9911/agent-kit/impl/session/inmemory"
)
