# ResourceLoader 设计(完整方案)

> 状态:设计待 review。目标:**一套** ResourceLoader 抽象统一所有资源
> 加载——配置(app/agent/namespace/component)、提示词、skill 包及其
> 文件读取能力(fs cap)——与部署环境解耦:仓库根、容器 `/`、systemd
> 工作目录、单二进制内嵌,同一份配置都能加载,路径语义一致可预测。

## 1. 全量资源清单(先把"要加载什么"列全)

| # | 资源 | 时机 | 读/写 | 当前锚点 | 问题 |
|---|---|---|---|---|---|
| 1 | app.yaml | 装配 | 读 | 调用方 path(相对 CWD) | CWD 依赖 |
| 2 | agent 文件(agents/*.yaml) | 装配 | 读 | 相对 app.yaml 目录 | ✅ 可移植 |
| 3 | namespace 文件(namespaces/*.yaml) | 装配 | 读 | 相对 agent 文件目录 | ✅ 可移植 |
| 4 | component 定义 | 装配 | 读 | **内联在 ns 文件**,无独立路径 | ✅ 随 ns |
| 5 | prompt 文件(`<dir>/<name>.md`) | 装配 | 读 | **相对 CWD** | 锚点分裂 |
| 6 | secrets 文件(file provider) | 装配 | 读 | 相对 CWD | 锚点分裂 |
| 7 | skill 包清单(SKILL.md) | 装配 | 读 | 安装目录(work_dir 下) | 只能本地盘 |
| 8 | **skill 包内文件(pack_read = fs cap)** | **运行** | 读 | 安装目录(work_dir 下) | 只能本地盘 |
| 9 | skill 包远程拉取 | 装配 | **写** | work_dir/.skills | 必须可写盘 ✅ |
| 10 | file 后端(session/todo/digest) | 运行 | **写** | 相对 CWD 的 dir | 必须可写盘 |
| 11 | 轨迹落盘 | 运行 | **写** | 配置的 path | 必须可写盘 |

**component 说明**:component 不是独立文件——它内联在 namespace 文件的
`components:` 列表里,只有 `prompt:`(可 `cap://prompt/` 引用文件)和
`tools:`(引用 sources)会间接触达资源 5/7。所以"加载 component"= 加载
它所在的 namespace 文件 + 解析它引用的 prompt/skill 资源,已被 3/5/7 覆盖。

**关键分野(设计的枢轴)**:第 1-8 行是**只读资源**(装配期 + fs cap 的
运行期读),天然能来自 embed / 远程 / 只读挂载;第 9-11 行是**可写运行
状态**,必须真实可写盘。现在两者都走裸 `os.*` 且部分锚 CWD,所以只读侧
被绑死在本地盘、锚点还分裂。

## 2. ResourceLoader:一个抽象,三类消费者

以 Go 标准库 `io/fs.FS` 为核心——它有免费适配器(`os.DirFS`、
`embed.FS`、测试 `fstest.MapFS`),整个生态都认。

### 2.1 契约(`protocol/resource`)

```go
package resource

// Loader 就是 fs.FS:Open 一个只读资源。os.DirFS / embed.FS / 远程
// 内存 FS 都天然满足,无需实现新接口。fs.FS 语义强制:'/' 分隔、
// 无卷标、拒绝 '..' 逃逸(顺带把 pack_read 的路径穿越防死)。
type Loader = fs.FS

// Resolve 把一个资源 ref 解析为「根 FS + FS 内的入口路径」。
// scheme 注册表:file(默认)、embed;第三方 http/oci/s3 自注册。
func Resolve(ref string) (root fs.FS, entry string, err error)

// Register 注册 scheme 解析器(代码注册、ref 按 scheme 启用)。
func Register(scheme string, r func(ref string) (fs.FS, string, error))
```

内置 scheme:
- `./app.yaml` / `/etc/app/app.yaml`(无 scheme)→ file:`os.DirFS(dir)` + base;
- `embed:main/config/app.yaml` → 宿主 `resource.RegisterEmbed("main", fs)` 注册的 embed.FS;
- `https://config.corp/app.yaml`(后置)→ http loader,拉取后以内存 FS 承载。

### 2.2 消费者一:配置(app/agent/namespace/component)

主入口从"吃 path"改为"吃 fs.FS":

```go
func LoadAppFS(root fs.FS, entry string) (*AppSpec, error)   // 新主入口

func LoadApp(ref string) (*AppSpec, error) {                 // 便捷包装,签名兼容
    root, entry, err := resource.Resolve(ref)
    if err != nil { return nil, err }
    return LoadAppFS(root, entry)
}
```

**统一锚点**:root FS 是唯一基准。agent 文件、namespace 文件全部解析为
**FS 内路径**(用 `path` 而非 `filepath`——fs.FS 一律 `/`、无 `..` 逃逸)。
现有"相对引用它的文件解析"的语义保留,只是基准从真实 FS 换成 root FS,
可移植性从"依赖 CWD"升为"依赖 root"。

单二进制内嵌一行搞定:

```go
//go:embed config
var cfgFS embed.FS
spec, _ := config.LoadAppFS(cfgFS, "config/app.yaml")   // 零磁盘依赖
```

### 2.3 消费者二:提示词(与配置同源)

file provider 不再自持 `dir` + 裸 `os.ReadFile`,改接收装配层传入的
**root FS 子树**:

```go
sub, _ := fs.Sub(root, promptDir)      // 配置根下的 prompts/
prompt.NewFileProvider(sub)            // Get: sub.Open(name+".md")
```

于是 prompt 与配置**天然同源**:配置在 embed 里,prompt 也在 embed;
配置在 `/etc/app` 里,prompt 自动在 `/etc/app/prompts`。锚点分裂消失。
secrets file provider 同理接 root FS。

### 2.4 消费者三:skill 包 + fs cap(pack_read)

skill 包有两种来源,ResourceLoader 统一承载"读"、只把"写"隔离:

- **bundled 包**(随配置分发,如 embed 里的 `skills/pdf/`)→ 直接
  `fs.Sub(root, packDir)`,**不 install、不落盘**,pack_read 从这个子树读;
- **remote 包**(`from: github.com/...`)→ 装配期 fetch 到 `state_dir`
  (可写盘,资源 9),再 `os.DirFS(installDir)` 得到 fs.FS,pack_read 从
  这个 FS 读。

关键:**pack_read(fs cap)持有的永远是一个 `fs.FS`**——不管包是内嵌、
本地盘、还是远程装下来的。所以:

```go
// pack_read 能力:构造时绑定 packFS(fs.FS 子树)
func packRead(packFS fs.FS) capability.Capability {
    // 调用:packFS.Open(path) —— fs.FS 语义禁止 '..' 逃出包根(免费的沙箱)
}
```

fs cap 因此**部署无关**:embed 的包、远程的包、本地的包,pack_read 一套
代码全支持;而且 fs.FS 的路径约束顺手把"读到包外用户文件"的穿越攻击
堵死(现在靠 `filepath` 手动校验 `[非法路径]`)。

### 2.5 可写状态:`state_dir` 显式化,与只读脱钩

`work_dir` 拆义(名实相符):
- 只读资源根 → 由 LoadAppFS 的 root FS 承载,配置不再为它指路径;
- 可写运行状态(资源 9/10/11)→ 顶层 `state_dir`(原 work_dir 可写语义)。
  skill 安装 / file 后端 / 轨迹落盘收口于此,**装配期校验可写,不可写即
  fail fast**(而非运行时才炸)。

默认链:`state_dir` → 环境 `AGENTKIT_STATE_DIR` → OS 约定
(`$XDG_STATE_HOME/agentkit` 或 `~/.local/state/agentkit`)。容器显式挂
volume 指过来。

### 2.6 入口搜索路径(消灭"自己算绝对路径")

给裸名字时的查找优先级:

1. 显式 `AGENTKIT_CONFIG`(绝对 / 带 scheme)——最高优先;
2. 进程 CWD;
3. 可执行文件所在目录(`os.Executable()`)——容器/systemd 友好;
4. `/etc/agentkit/`(系统级)。

命中即用,启动日志明示"从哪加载",不静默。示例里的
`if os.Stat(appPath)!=nil { appPath="app.yaml" }` 兜底 hack 删除。

## 3. 分层与放置

- 新增 `protocol/resource`:`fs.FS` 之上的最小契约 + scheme 注册表。
  协议层,零业务、零 impl 依赖。
- `impl/resource/embed`(内置)、`impl/resource/http`(后置)自注册。
- `config` 依赖 `protocol/resource`,`LoadAppFS` 为主入口。
- `protocol/prompt` file/secrets provider 从"自持 dir"改"接 fs.FS 子树"。
- `skill` 的 pack_read 从"持 dir"改"持 fs.FS";fetch(写)仍走 state_dir。
- 分层守卫不破:resource 在 protocol 层,config/prompt/skill 在其上消费。

## 4. 迁移(pre-1.0 硬切,报错内嵌新写法)

| 变更 | 旧 | 新 | 破坏面 |
|---|---|---|---|
| 可写目录 | `work_dir` | `state_dir` | 旧键装配期报错指路 |
| 只读根 | work_dir 兼只读根 | root FS 承载,配置无需指路径 | prompt/secrets 基准 CWD→配置根(修 bug) |
| 主入口 | `LoadApp(path)` | `LoadAppFS(fsys, entry)`(LoadApp 保留) | 无(便捷签名兼容) |
| pack_read | 持安装 dir | 持 packFS(fs.FS) | 内部,配置无感;顺带修穿越 |

## 5. 实施批次

| 批 | 内容 | 规模 |
|---|---|---|
| 1 | `protocol/resource`:fs.FS 契约 + file/embed scheme + 注册表 + 入口搜索路径(fstest.MapFS 覆盖内嵌/远程/穿越) | 半天 |
| 2 | `config.LoadAppFS` 主入口 + app/agent/ns 解析改 root FS 内 `path` 锚点;LoadApp 降包装 | 1 天 |
| 3 | prompt/secrets file provider 改吃 fs.FS 子树;装配层接线同源 | 半天 |
| 4 | skill pack + pack_read(fs cap)改 fs.FS:bundled 走 fs.Sub、remote 走 state_dir;穿越用 fs.FS 语义兜底 | 1 天 |
| 5 | `work_dir`→`state_dir` 硬切 + 可写校验 fail fast;示例删 os.Stat hack + 新增 examples/embedded(单二进制内嵌)+ README 资源加载章节 | 半天 |

批 2/4 动核心装配与能力路径;其余增量。全程 fstest.MapFS + 临时目录做
端到端(内嵌路径、远程 FS、可写校验、pack_read 穿越),不涉及模型。

## 6. 需要裁决的点

1. `work_dir`→`state_dir` 改名:硬切(推荐,名实相符 + 只读/可写分野)vs 保留 work_dir 只加只读抽象;
2. http/oci loader 本批做 vs 先只做 file+embed(推荐,覆盖 99%,远程后置);
3. 入口搜索路径要不要(推荐要);
4. fs cap 是否扩展为**通用文件工具**(不止 pack_read,给 agent 一个受
   root FS 约束的读文件能力)——推荐先只把 pack_read 归入抽象,通用
   fs 工具作为登记项(有需求再开,天然复用同一 fs.FS 沙箱)。
