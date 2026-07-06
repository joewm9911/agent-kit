// examples/smoke/run:把冒烟场景的 ops-manager 拉起为**可交互 agent**。
//
//	MINIMAX_API_KEY=$(security find-generic-password -a agent-kit -s minimax-api-key -w) \
//	go run ./examples/smoke/run
//
// 与测试驱动的区别:这里是真人对话——业务后端(商品/库存/价格/客户)由
// 本进程内置 mock 提供,模型是真实 MiniMax,pdf 技能默认拉真实的
// anthropics/skills(可用 PDF_SKILL_REF 覆盖)。ask_user 与审批走终端。
//
// 试试:
//
//	> 用 quick-product-qa 查降噪耳机价格
//	> 给 P100 做完整定价审查
//	> 提取 /tmp/x.pdf 的文本(pdf 技能;需 python3 + pypdf)
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/joewm9911/agent-kit/impl/model/minimax"
	_ "github.com/joewm9911/agent-kit/impl/source/exectool" // pdf 技能的脚本执行
	_ "github.com/joewm9911/agent-kit/impl/source/httptool"
	_ "github.com/joewm9911/agent-kit/impl/source/vector"
	_ "github.com/joewm9911/agent-kit/std"

	"github.com/cloudwego/eino/callbacks"

	"github.com/joewm9911/agent-kit/config"
	"github.com/joewm9911/agent-kit/impl/interactor/cli"
	"github.com/joewm9911/agent-kit/runtime/loop"
	"github.com/joewm9911/agent-kit/runtime/observe"
)

// mockBackend 是内置业务后端:与 config 包冒烟测试同一形状的五个端点。
func mockBackend() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/products", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"id":"P100","name":"降噪耳机","price":129}]`)
	})
	mux.HandleFunc("/products/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":"P100","name":"降噪耳机","price":129,"cost":80}`)
	})
	mux.HandleFunc("/inventory/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"sku":"P100","warehouses":%q}`, strings.Repeat("仓A:120;仓B:88;", 400))
	})
	mux.HandleFunc("/price", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	})
	mux.HandleFunc("/customers/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":"C1","tier":"VIP","note":"多次咨询降噪耳机"}`)
	})
	return httptest.NewServer(mux)
}

// setDefault 只在未设置时注入默认值(已导出的环境不覆盖)。
func setDefault(key, val string) {
	if os.Getenv(key) == "" {
		os.Setenv(key, val)
	}
}

func main() {
	if os.Getenv("MINIMAX_API_KEY") == "" {
		log.Fatal("需要 MINIMAX_API_KEY(keychain: security find-generic-password -a agent-kit -s minimax-api-key -w)")
	}
	srv := mockBackend()
	defer srv.Close()

	setDefault("SMOKE_MODEL_PROVIDER", "minimax")
	setDefault("SMOKE_MODEL_BASE", "https://api.minimaxi.com/v1")
	setDefault("SMOKE_API_BASE", srv.URL)
	setDefault("SMOKE_DATA_DIR", "./data/smoke") // work_dir:技能装 <此目录>/agent-kit/.skills
	// pdf 技能:默认真实 anthropics/skills(pin);离线/自定义用 PDF_SKILL_REF 覆盖
	setDefault("PDF_SKILL_REF", "github.com/anthropics/skills/skills/pdf@9d2f1ae187231d8199c64b5b762e1bdf2244733d")

	// 配置树相对仓库根;支持从仓库根或本目录运行
	appPath := "examples/smoke/app.yaml"
	if _, err := os.Stat(appPath); err != nil {
		appPath = "../app.yaml"
	}
	spec, err := config.LoadApp(appPath)
	if err != nil {
		log.Fatal(err)
	}
	// 冒烟树的压缩阈值是为测试强制触发调的(12/4),交互长任务里会中途
	// 压掉工作上下文导致模型丢失工具调用惯性;交互模式放宽。
	for _, as := range spec.Agents {
		if as.Name == "ops-manager" {
			as.Loop.Compaction = &loop.CompactionConfig{MaxMessages: 40, KeepRecent: 10}
		}
	}
	app, err := config.BuildApp(context.Background(), spec, config.BuildOptions{Interactor: cli.NewCLI()})
	if err != nil {
		log.Fatal(err)
	}
	ag := app.Agents["ops-manager"]
	if ag == nil {
		log.Fatal("ops-manager not built")
	}

	// 进度切面:每一步模型/工具调用可见(含技能子循环内部)
	callbacks.AppendGlobalHandlers(observe.Progress(os.Stdout))

	skillsDir, _ := filepath.Abs(filepath.Join(os.Getenv("SMOKE_DATA_DIR"), "agent-kit", ".skills"))
	fmt.Printf("ops-manager ready(模型: 真实 MiniMax;业务后端: 内置 mock;技能安装: %s)\n", skillsDir)
	fmt.Println("提示:pdf 技能跑脚本需要 python3 + pypdf(pip install pypdf);输入 exit 退出。")
	fmt.Println("挂载能力:")
	if mounted := app.AgentMounts["ops-manager"]; mounted != nil {
		for _, m := range mounted.List() {
			fmt.Printf("  %-50s risk=%s\n", m.Ref, m.Risk)
		}
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
