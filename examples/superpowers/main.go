// examples/superpowers:把 obra/superpowers 技能集拉起为**可交互的研发教练**
// (smoke.yaml 保持纯测试用途,本 runner 独立)。真实 MiniMax,首启从 GitHub
// 下载三个真实技能到 <work_dir>/agent-kit/.skills,按你的问题路由到对应技能。
//
//	MINIMAX_API_KEY=$(security find-generic-password -a agent-kit -s minimax-api-key -w) \
//	go run ./examples/superpowers
//
// 试试:
//
//	> 我的 Go 测试 TestX 偶发失败,重跑就过,帮我排查
//	> 我要给一个折扣函数写测试,第一步做什么
//	> 帮我把"离线优先的同步"这个想法梳理成方案
//
// 带脚本的技能(brainstorming/systematic-debugging)是 Dangerous 风险:每次
// 调用会在终端请求审批(y 放行,a 本会话免问)。技能脚本需 python3/node。
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/joewm9911/agent-kit/impl/model/minimax"
	_ "github.com/joewm9911/agent-kit/impl/source/exectool" // 技能脚本执行
	_ "github.com/joewm9911/agent-kit/std"

	"github.com/cloudwego/eino/callbacks"

	"github.com/joewm9911/agent-kit/config"
	"github.com/joewm9911/agent-kit/impl/interactor/cli"
	"github.com/joewm9911/agent-kit/protocol/resource"
	"github.com/joewm9911/agent-kit/runtime/observe"
)

func setDefault(key, val string) {
	if os.Getenv(key) == "" {
		os.Setenv(key, val)
	}
}

func main() {
	if os.Getenv("MINIMAX_API_KEY") == "" {
		log.Fatal("需要 MINIMAX_API_KEY(keychain: security find-generic-password -a agent-kit -s minimax-api-key -w)")
	}
	setDefault("SP_MODEL_BASE", "https://api.minimaxi.com/v1")
	setDefault("SP_WORK_DIR", "./data/superpowers")

	// 入口经 resource.Find 搜索(AGENTKIT_CONFIG → CWD → 可执行文件目录
	// → /etc/agentkit),不再手写 os.Stat 兜底。
	ref, err := resource.Find("examples/superpowers/interactive.yaml")
	if err != nil {
		ref = "interactive.yaml"
	}
	cfg, err := config.Load(ref)
	if err != nil {
		log.Fatal(err)
	}
	app, err := config.Build(context.Background(), cfg, config.BuildOptions{Interactor: cli.NewCLI()})
	if err != nil {
		log.Fatal(err)
	}
	ag := app.Agents["coach"]
	if ag == nil {
		log.Fatal("coach not built")
	}

	callbacks.AppendGlobalHandlers(observe.Progress(os.Stdout))

	// 会话按进程隔离(session ID 才是会话身份);SP_SESSION 显式指定可续聊。
	sessionID := os.Getenv("SP_SESSION")
	if sessionID == "" {
		sessionID = fmt.Sprintf("cli-%d", os.Getpid())
	}

	skillsDir, _ := filepath.Abs(filepath.Join(os.Getenv("SP_WORK_DIR"), "agent-kit", ".skills"))
	fmt.Printf("研发教练 ready(模型: MiniMax;会话: %s;技能安装: %s)\n", sessionID, skillsDir)
	fmt.Println("提示:技能脚本需 python3/node;带脚本的技能调用需终端审批;输入 exit 退出。")
	fmt.Println("挂载技能:")
	for _, m := range app.Catalog.List() {
		fmt.Printf("  %-42s risk=%s\n", m.Ref, m.Risk)
	}

	scanner := bufio.NewScanner(os.Stdin)
	ctx := context.Background()
	for {
		fmt.Print("\n> ")
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
		answer, err := ag.Run(ctx, sessionID, input)
		if err != nil {
			fmt.Println("error:", err)
			continue
		}
		fmt.Println("\n" + answer)
	}
}
