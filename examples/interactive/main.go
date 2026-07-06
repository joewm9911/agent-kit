// examples/interactive:冒烟场景的**可交互副本**——smoke 树保持纯测试用途,
// 本目录独立演进。真实 MiniMax + 内置 mock 业务后端 + 真实 pdf 技能。
//
//	MINIMAX_API_KEY=$(security find-generic-password -a agent-kit -s minimax-api-key -w) \
//	go run ./examples/interactive
//
// 试试:
//
//	> 用 quick-product-qa 查降噪耳机价格
//	> 给 P100 做完整定价审查
//	> 我们在卖哪些产品?汇总整理生成一份 PDF 汇报(pdf 技能;需 python3 + pypdf)
//	> 用 python 算一下 P100 提价 8% 后毛利率是多少,顺便打印 platform 看跑在哪
//
// 脚本执行与沙箱:app.yaml 的 exec: 块是 app 级默认沙箱策略,覆盖计算域的
// python 工具与 pdf 技能包的脚本。本 runner 启动时检测 docker:
//
//	有 docker  → OPS_SANDBOX=docker,脚本进一次性加固容器(python 打印
//	             platform 会看到 Linux——证明不在宿主上跑);
//	无 docker  → 宿主直跑并打印告警(装 docker 或 export OPS_SANDBOX=docker 强制)。
//
// 默认镜像 python:3.12-slim(公共镜像,纯计算够用);要让 pdf 技能的脚本
// 也进沙箱,build examples/exec-runtime/Dockerfile(预装 pypdf/nodejs)后
// export OPS_SANDBOX_IMAGE=agent-kit-runtime:latest。
//
// pdf 技能与 python 工具都是 Dangerous 风险:每次调用会在终端请求批准
// (y 放行,a 本会话免问)。
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"

	_ "github.com/joewm9911/agent-kit/impl/exec/docker" // 注册 docker 沙箱(exec.default_sandbox: docker)
	_ "github.com/joewm9911/agent-kit/impl/model/minimax"
	_ "github.com/joewm9911/agent-kit/impl/source/exectool" // pdf 技能与计算域的脚本执行
	_ "github.com/joewm9911/agent-kit/impl/source/httptool"
	_ "github.com/joewm9911/agent-kit/impl/source/vector"
	_ "github.com/joewm9911/agent-kit/std"

	"github.com/cloudwego/eino/callbacks"

	"github.com/joewm9911/agent-kit/config"
	"github.com/joewm9911/agent-kit/impl/interactor/cli"
	"github.com/joewm9911/agent-kit/runtime/observe"
)

// mockBackend 是内置业务后端(商品/库存/价格/客户),与冒烟测试同形。
func mockBackend() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/products", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"id":"P100","name":"降噪耳机","price":129},{"id":"P200","name":"机械键盘","price":399}]`)
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

// setDefault 只在未设置时注入默认值(外部已导出的环境优先)。
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

	setDefault("OPS_MODEL_BASE", "https://api.minimaxi.com/v1")
	setDefault("OPS_API_BASE", srv.URL)
	setDefault("OPS_DATA_DIR", "./data/interactive") // 技能装 <此目录>/agent-kit/.skills
	setDefault("PDF_SKILL_REF", "github.com/anthropics/skills/skills/pdf@9d2f1ae187231d8199c64b5b762e1bdf2244733d")

	// 沙箱注入:有 docker 就默认全部脚本进容器;没有则宿主直跑并告警
	// (装配不静默——启动横幅明示脚本跑在哪)。生产应写死 default_sandbox
	// 并开 require_sandbox,见 app.yaml 注释。
	sandboxNote := ""
	if _, err := osexec.LookPath("docker"); err == nil {
		setDefault("OPS_SANDBOX", "docker")
	} else {
		setDefault("OPS_SANDBOX", "")
	}
	setDefault("OPS_SANDBOX_IMAGE", "python:3.12-slim")
	if os.Getenv("OPS_SANDBOX") == "" {
		sandboxNote = "宿主直跑(未检测到 docker;安装 docker 或 export OPS_SANDBOX=docker 后脚本进加固容器)"
	} else {
		sandboxNote = fmt.Sprintf("%s(镜像 %s;脚本在一次性加固容器里跑,宿主不可见)",
			os.Getenv("OPS_SANDBOX"), os.Getenv("OPS_SANDBOX_IMAGE"))
	}

	// 配置树相对仓库根;也支持从本目录运行
	appPath := "examples/interactive/app.yaml"
	if _, err := os.Stat(appPath); err != nil {
		appPath = "app.yaml"
	}
	spec, err := config.LoadApp(appPath)
	if err != nil {
		log.Fatal(err)
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

	// 会话按进程隔离:每次启动都是新会话(session ID 才是会话身份,file
	// 后端跨进程持久,写死 ID 会导致"重启=续聊")。想跨重启续聊,显式
	// OPS_SESSION=<上次的会话 ID>(历史见 <OPS_DATA_DIR>/ops-sessions/)。
	sessionID := os.Getenv("OPS_SESSION")
	if sessionID == "" {
		sessionID = fmt.Sprintf("cli-%d", os.Getpid())
	}

	skillsDir, _ := filepath.Abs(filepath.Join(os.Getenv("OPS_DATA_DIR"), "agent-kit", ".skills"))
	fmt.Printf("ops-manager ready(模型: MiniMax;业务后端: 内置 mock;会话: %s;技能安装: %s)\n", sessionID, skillsDir)
	fmt.Printf("脚本沙箱:%s\n", sandboxNote)
	fmt.Println("提示:宿主直跑时 pdf 技能需要 python3 + pypdf(pip install pypdf);输入 exit 退出。")
	fmt.Println("挂载能力:")
	if mounted := app.AgentMounts["ops-manager"]; mounted != nil {
		for _, m := range mounted.List() {
			fmt.Printf("  %-50s risk=%s\n", m.Ref, m.Risk)
		}
	}
	// 全局源直挂的工具(capabilities.include 选品,不在 AgentMounts 里)
	for _, m := range app.Catalog.List() {
		if m.Ref.Kind == "tool" && m.Ref.Domain == "calc" {
			fmt.Printf("  %-50s risk=%s\n", m.Ref, m.Risk)
		}
	}
	// 内建能力不入目录,随 agent 装配自动挂载,横幅明示避免误判"没加载"
	fmt.Println("内建能力:todo_write/todo_read(计划)、ask_user(追问)、memory_save/memory_search(长期记忆)、read_result(大结果取回)")

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
