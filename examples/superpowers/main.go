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

	appPath := "examples/superpowers/interactive.yaml"
	if _, err := os.Stat(appPath); err != nil {
		appPath = "interactive.yaml"
	}
	cfg, err := config.Load(appPath)
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

	skillsDir, _ := filepath.Abs(filepath.Join(os.Getenv("SP_WORK_DIR"), "agent-kit", ".skills"))
	fmt.Printf("研发教练 ready(模型: MiniMax;技能安装: %s)\n", skillsDir)
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
		answer, err := ag.Run(ctx, "cli", input)
		if err != nil {
			fmt.Println("error:", err)
			continue
		}
		fmt.Println("\n" + answer)
	}
}
