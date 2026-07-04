// examples/main.go:从一份 YAML 拉起整个应用。
//   - serving.addr 未配置 → CLI REPL(ask_user/审批走终端);
//   - serving.addr 配置了 → Gateway 模式(HTTP/SSE + A2A + IM webhook)。
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	// 空导入触发 source / channel / model 工厂注册
	_ "github.com/joewm9911/agent-kit/channel/feishu"
	_ "github.com/joewm9911/agent-kit/provider/a2a"
	_ "github.com/joewm9911/agent-kit/provider/httptool"
	_ "github.com/joewm9911/agent-kit/provider/mcptool"
	_ "github.com/joewm9911/agent-kit/provider/models"
	_ "github.com/joewm9911/agent-kit/provider/vector"

	"github.com/cloudwego/eino/callbacks"

	"github.com/joewm9911/agent-kit/capability"
	"github.com/joewm9911/agent-kit/config"
	"github.com/joewm9911/agent-kit/interact"
	"github.com/joewm9911/agent-kit/observe"
	"github.com/joewm9911/agent-kit/provider/local"
)

func main() {
	ctx := context.Background()

	// 代码侧能力:Go 函数泛型推断 schema,以 local source 入目录。
	type nowReq struct{}
	now, err := local.Func("current_time", "获取当前时间",
		func(ctx context.Context, _ *nowReq) (string, error) {
			return time.Now().Format(time.RFC3339), nil
		})
	if err != nil {
		log.Fatal(err)
	}

	// 多文件形态(app.yaml + agents/ + namespaces/)优先;
	// 单文件 agent.yaml 是兼容路径。
	opts := config.BuildOptions{
		Interactor:        interact.NewCLI(),
		ExtraCapabilities: []capability.Capability{now},
	}
	var app *config.App
	if _, statErr := os.Stat("app.yaml"); statErr == nil {
		spec, err := config.LoadApp("app.yaml")
		if err != nil {
			log.Fatal(err)
		}
		if app, err = config.BuildApp(ctx, spec, opts); err != nil {
			log.Fatal(err)
		}
	} else {
		cfg, err := config.Load("agent.yaml")
		if err != nil {
			log.Fatal(err)
		}
		if app, err = config.Build(ctx, cfg, opts); err != nil {
			log.Fatal(err)
		}
	}

	// Gateway 模式
	if app.Server != nil {
		log.Fatal(app.Server.Run(ctx))
	}

	// CLI 模式:进度提示(全局切面,skill 内部步骤也可见)+ 流式输出
	callbacks.AppendGlobalHandlers(observe.Progress(os.Stdout))

	ag := app.Agents["assistant"]
	if ag == nil {
		log.Fatal("agent 'assistant' not found")
	}
	fmt.Println("能力目录:")
	for _, m := range app.Catalog.List() {
		fmt.Printf("  %-55s risk=%s\n", m.Ref, m.Risk)
	}
	fmt.Printf("\nagent %q ready。输入 exit 退出。\n", ag.Name())

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}
		if input == "exit" {
			break
		}
		// 用 Run 而非 Stream:invoke 范式下进度切面能完整看到每一步
		// (工具入参/结果、模型决策);回答在结束时整段打印。
		answer, err := ag.Run(ctx, "cli-session", input)
		if err != nil {
			fmt.Println("error:", err)
			continue
		}
		fmt.Println("\n" + answer)
	}
}
