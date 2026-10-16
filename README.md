# 🐋 OrcaTerm AI 网关 (orcaterm2api)

**Version 1.0.1**

*单二进制 + 一个 YAML 配置 · 无数据库 · 零第三方依赖 · 把 OrcaTerm（腾讯遨驰终端）内置 AI 助手的账号池、限速冷却、跨号重试和 SOCKS5 出口打包成一个进程，对外暴露 OpenAI / Anthropic / Responses 三套接口。*

> 非官方项目，与腾讯云、OrcaTerm 官方无关。仅限本人授权账号、本机 / 私有环境学习研究使用；使用前请确认符合 OrcaTerm 服务条款。

- [English README](README_EN.md)

## 特性一览

- **三套对外接口**：OpenAI `/v1/chat/completions`、Anthropic `/v1/messages`、OpenAI Responses `/v1/responses`，全部支持流式 SSE
- **多账号池**：加权随机选号、单号在途上限、429/5xx 冷却、连续失败熔断（指数退避）、登录态失效自动停用
- **双凭证 + 一键收号**：桌面端 `ot_session` 与浏览器控制台态 `uin/skey` 混池运行；面板生成一次性配对码与书签脚本，登录过 Web 版的页面点一下即自动收号（落盘前实测凭据）
- **跨号重试**：单请求按 `retry-on` 状态码自动换号，上游语义保留（429 透传 429，不会伪装成 502）
- **会话粘性**：`X-Session-Id` 等键固定「账号 + 上游会话」，多轮上下文不跳号，`system` 只在首轮生效
- **工具调用闭环**：上游 `review` 转 OpenAI `tool_calls`，结果按 OrcaTerm 真机格式回填同一会话
- **SOCKS5/HTTP 代理池**：自研 dialer（无认证 / 用户名密码），轮询 / 随机 / 粘号策略，健康检查 + 失败冷却，支持按账号绑定专用代理
- **Web 管理面板**：零依赖单文件页面，账号增删改、批量导入、下游密钥、代理池、用量统计、网关设置全部在线操作
- **热重载**：配置文件变更自动生效（账号、代理、池参数、上游地址），无需重启
- **安全默认**：未设置面板密码时管理面板与 pprof 仅限本机回环访问；密码仅经请求头传递

## 上游协议

网关把 OrcaTerm 内置 AI 助手封装为 OpenAI 兼容 API。上游协议来自对 OrcaTerm 桌面客户端的抓包逆向（感谢 [cirnosalt/orcabridge](https://github.com/cirnosalt/orcabridge) 的协议研究）：

- `POST https://lightai.cloud.tencent.com/assistant/chat?name=orcaterm&mode=0&csrfCode=&sseresume=true`
- 鉴权：`Authorization: Bearer <ot_session>` + Cookie（`ot_session` / `sid` / `userId`）
- 会话：`conversationId` 为 `cid-<uuid>-orcaterm` 格式，历史由服务端保存
- 模型：18 个（8 current + 9 legacy + 1 别名），默认 `deepseek-v4-flash`
- 工具：上游 `action=review` + `data.tool` 转 OpenAI `tool_calls`；结果按真机格式回填
- 登录态失效：HTTP 200 + `code=10050000` → 网关停用该账号并自动换号重试

## 快速开始

### 方式一：Docker Compose（推荐）

```
git clone https://github.com/ZFXing-lite/orcaterm2api.git
cd orcaterm2api
cp config.example.yaml config.yaml
# 编辑 config.yaml，设置 remote-management.secret-key（面板密码）
ORCATERM2API_MGMT_KEY=你的面板密码 docker compose up -d --build
```

浏览器打开 `http://服务器IP:8317/v0/management/panel`，用密码登录，在「上游账号」页添加账号。

> 默认 `ORCATERM2API_ALLOW_REMOTE=0`（仅本机访问面板）。需要从外部访问面板时设置 `ORCATERM2API_ALLOW_REMOTE=1` 并**务必设置面板密码**。**未设置面板密码时，无论该开关为何值，管理面板与 `/debug/pprof` 都只接受本机回环访问。**

### 方式二：本机编译运行

要求 Go ≥ 1.22：

```
go build -ldflags="-s -w" -o orcaterm2api ./cmd/server
cp config.example.yaml config.yaml
ORCATERM2API_MGMT_KEY=你的面板密码 ./orcaterm2api -c config.yaml
```

### 安装后：三步上线

1. 浏览器访问 `http://127.0.0.1:8317/v0/management/panel`
2. 输入面板密码登录
3. 在「上游账号」页添加账号 Cookie（见下），在「网关设置」确认默认模型

没填上游账号时 `/healthz` 返回 `ready: false`，填上后变为 `ready: true`。

## 获取账号 Cookie

每个上游账号 = 一个 OrcaTerm 登录态，**两种形态任选**：

| 形态 | Cookie 关键字段 | 获取方式 |
|---|---|---|
| 桌面端（OAuth） | `ot_session`（JWT，`sid` 可选） | 桌面客户端开发者工具 → Network → `assistant/chat` 请求头；或 `%LOCALAPPDATA%\com.orcaterm-desktop.app\.cookies` |
| 浏览器控制台态 | `uin` + `skey` + `userId` | 登录 [OrcaTerm Web 版](https://orcaterm.cloud.tencent.com)后，浏览器 F12 控制台执行 `copy(document.cookie)` |

- 两种形态可混在一个账号池里跑，网关按凭证类型自动选择鉴权方式（`Bearer ot_session` 或 cookie 重放），并同步切换请求指纹（桌面态=客户端 Origin/UA，控制台态=Web 版 Origin/UA/浏览器头集），与真实流量对齐。
- 控制台态没有独立过期时间，随腾讯云登录失效（skey 寿命数小时级）；失效时上游返回 `NEED_UIN_AND_SKEY` 或 `VERIFY_SKEY_UIN_FAILED`，与桌面态 `code=10050000` 一样触发自动停用 + 换号重试——过期后回面板点一次书签重新收号即可。

### 一键收号（书签，推荐）

面板「上游账号」页 → **一键收号** → 生成配对码与配套书签 → 把书签拖进浏览器收藏栏 → 在任意已登录 OrcaTerm Web 版的页面点一下书签，登录态自动回传网关：

- 落盘前网关会**先实测凭据有效性**（调上游只读接口），坏票直接拒收并返回原因
- 配对码 15 分钟内一次性有效；收号端点以配对码为凭证，无需管理密钥
- 本机部署（`http://127.0.0.1:*`）开箱即用；远程部署建议给网关上 https（浏览器允许 https 页面请求 localhost，拦截跨域 http）

在面板「上游账号」页粘贴，支持批量导入（每行 `名称----cookie`）。

> 上游没有刷新接口，登录态过期后重新收号即可。

## 客户端接入

| 客户端 | Base URL | 接口 |
| --- | --- | --- |
| OpenAI SDK / curl | `http://host:8317/v1` | `/v1/chat/completions` |
| Anthropic SDK / Claude Code | `http://host:8317` | `/v1/messages`（`x-api-key`） |
| Codex / Qoder | `http://host:8317/v1` | `/v1/responses` |

curl 示例：

```
export OPENAI_API_KEY=<你在面板设置的下游密钥>
curl -X POST http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}'
```

`GET /v1/models` 返回 18 个模型条目 + 兼容别名 `orcaterm-assistant`。

### 会话粘性

带 `X-Session-Id` 请求头（或 OpenAI `user` 字段 / `metadata.conversation_id`）可复用上游会话并**固定走同一账号**：多轮上下文不跳号。`system` 消息只在新会话首轮生效（上游语义），复用会话时会被忽略。

### 流式

`stream: true` 返回网关合成的 SSE（上游以非流式调用，网关完整解析后再合成流，逐块实时冲刷）。响应头在拿到上游结果后才提交，首字节前的上游失败已在内部重试/换号。`stream_options.include_usage` 可在末尾追加 usage 块（本地估算）。

### 工具调用

声明的工具名应为 OrcaTerm 原生工具（`fetch`、`execute_terminal_command`、`list_terminals`、`run_commands`、`submit_agent_tasks`、`read_cloud_space_file` 等）。网关把上游 `review` 转为 OpenAI `tool_calls`，由客户端执行后回传结果，网关按真机格式回填同一会话。

限制（上游能力边界，会显式 400 而不是静默忽略）：

- 不能注册任意自定义 function（上游只认原生工具名；声明了上游没有的名字会如实列在 `X-OrcaTerm-Unsupported-Tools` 响应头）
- 不支持 `tool_choice: "required"`、具名强制选择（含 `{"type":"tool"}` 形态）、`parallel_tool_calls: true`、一次回传多个工具结果
- 上游 `ask` 动作（反问确认）是正常 200、`finish_reason` 为 `stop`，真实动作记录在 `X-OrcaTerm-Action: ask` 响应头

## 配置文件

`config.yaml` 不存在会自动生成，必须可写（面板修改会回写）。环境变量覆盖同名配置项。配置文件变更会热重载（账号、代理、限速参数、上游地址全部跟随），无需重启。完整字段见 [`config.example.yaml`](config.example.yaml)。

常用环境变量：

| 变量 | 说明 |
| --- | --- |
| `ORCATERM2API_MGMT_KEY` | 面板密码（覆盖 `remote-management.secret-key`） |
| `ORCATERM2API_ALLOW_REMOTE` | `1`=允许远程访问面板（要求已设置面板密码，否则恒为仅本地） |
| `ORCATERM2API_API_KEYS` | 下游密钥，逗号分隔 |
| `ORCATERM2API_BASE_URL` | 上游地址覆盖 |
| `ORCATERM2API_DEFAULT_MODEL` | 默认模型覆盖 |
| `ORCATERM2API_PORT` | 监听端口覆盖 |

## 安全模型

- **下游调用**：`api-keys` 为空 = 免鉴权（本机 / 内网谨慎使用）；配置后走 `Authorization: Bearer` 或 `x-api-key`
- **管理面板与运维 API**：密码经 `X-Mgmt-Key` / `Authorization` 请求头校验，不接受 URL query 传密码；密码为空时**只接受本机回环连接**
- **诊断端点**：`/debug/pprof` 与管理面板同权限，公网默认不可达
- **落盘**：配置与状态文件以 0600 权限原子写入（tmp + rename），上游 Cookie 不出现在日志与 `/status` 快照中

## 管理面板

`http://127.0.0.1:8317/v0/management/panel`，零依赖单文件页面，每 5 秒自动刷新：

- **概览**：状态、总请求、错误率、上游账号数、代理数、默认模型
- **上游账号**：每个账号的状态/冷却原因/登录过期时间/在途/成功失败计数，一键停用/启用/删除，表单添加，批量导入
- **下游密钥**：增删客户端密钥，删除即刻失效
- **代理池**：节点健康/成功/失败计数，按行粘贴整池替换
- **接口统计**：每个 `/v1/` 路径的请求/错误/在途/最近状态码
- **Token 用量**：按账号、按模型汇总（本地估算），落盘 `state/usage.json`
- **网关设置**：默认模型、代理策略、上游地址等，保存立即生效

管理 API 在 `/v0/management/api/*`（需 `X-Mgmt-Key`）。

## 账号池行为

- **选号**：按有效权重排序取 top 5，加权随机抽签；权重相同时 LRU 打破平局；全部冷却中时兜底选最快恢复的那个（仍受单号并发上限约束）
- **在途租约**：单账号最大在途请求数（`rate-limit.max-in-flight`），占满的号不参与选号
- **冷却**：429（认 `Retry-After`，封顶 10m，不叠加惩罚）、5xx、连续失败熔断（指数退避封顶 6h）
- **登录失效**：`code=10050000` 停用该账号（可在面板重新启用）并自动换下一个号完成当前请求；上游无刷新接口，需人工更新 Cookie
- **传输失败**（代理挂/超时）：账号临时降级 `err-cooldown`，不记为账号污点
- **持久化**：池状态与用量原子写入 `state/`（tmp + rename，0600），重启自动恢复

## 接口一览

| 路径 | 说明 |
| --- | --- |
| `POST /v1/chat/completions` | OpenAI Chat Completions（含流式） |
| `POST /v1/messages` | Anthropic Messages（含流式） |
| `POST /v1/responses` | OpenAI Responses（含流式） |
| `GET /v1/models` | 18 模型目录 + 别名 |
| `GET /healthz` | 存活探针；`ready` 表示是否有可用上游账号 |
| `GET /status` | 脱敏的池状态与用量快照（需下游密钥） |
| `*/v0/management/panel` | Web 管理面板 |
| `*/v0/management/*` | 运维 API（需面板密码） |

## 目录结构

```
orcaterm2api/
├── cmd/server/main.go        # 入口
├── internal/
│   ├── config/               # YAML 配置（自研解析器，零依赖）+ 热重载 + 原子回写
│   ├── orca/                 # OrcaTerm 上游协议客户端（鉴权/会话/模型/工具）
│   ├── keypool/              # 上游账号池（轮询/冷却/熔断/重试/持久化）
│   ├── proxypool/            # SOCKS5/HTTP 代理池（自研 dialer，零依赖）
│   ├── relay/                # 编排：会话粘性/工具循环/OpenAI 响应/SSE 合成
│   ├── metrics/              # Token 用量（本地估算）+ 接口统计
│   └── server/               # HTTP 路由 + Anthropic/Responses 适配 + 管理面板
├── config.example.yaml
├── docker-compose.yml
├── Dockerfile
└── README.md / README_EN.md
```

## 开发

```
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

## 更新日志

### v1.1.4

**无人值守模式（默认开，`upstream.no-ask`）**

- 解决上游助手「每轮对话默认反问用户」的问题：向 prompt 首块注入行为指令——无人值守、默认直接执行、信息不足时自行假设并说明；**仅当用户明确表示想讨论/征求意见时才允许提问**（语义判断交给上游模型，网关不做关键词拦截）
- 非流式请求附带自动确认兜底：上游仍然反问时，网关在同会话自动回填「直接执行」一轮拿最终结果；确认后仍反问则把问题原样交给下游
- 流式请求靠提示词抑制（字节已飞无法二次确认），残余反问按原文透传
- 面板「网关设置」新增开关：无人值守（抑制上游反问）
- 顺带加固：流式字段提取器跳过空值字段（ask 响应里 `"taskCompletion":""` 在前不再吞掉 question）；协议解析对「模型先说话再给 JSON」的前置噪声免疫

### v1.1.3

- 修复真上游实测出的解析问题：上游模型生成协议 JSON 时会习惯性包裹 markdown 围栏（```json … ```），导致聚合层报「解析上游 JSON 失败: invalid character '`'」。解析层现在自动剥离围栏与前后噪声、并按最外层花括号跨度兜底提取；流式透传本就不受影响（围栏在字段标记前被吞），两种模式均已实测验证

### v1.1.2

**流式输出与长文本根治**

- 上游传输层切真流式（`stream:true` SSE 分块读取），绕过 CDN 对单体响应的大小/时长限制——长文本输出不再截断，超时改为「块间空闲 90s 判死」而非总时长硬掐
- `stream:true` 请求现在是真打字机：上游吐多快，下游就到多快（流式字段提取器实时还原 `taskCompletion`/`question` 可见文本，跨 token 的转义/标记劈开均已处理）
- 下游收到首段文本之前上游失败仍自动换号重试（重试窗口语义保持）
- 工具调用（review）无可见文本场景由流末聚合兜底，兼容信封/内层两种协议形态与宽松/紧凑两种序列化风格
- 「上游模型」页探活提效：先预检账号登录态，失效短路返回（不再空跑 17 次）；单模型 15s 短超时 + 并发 4 + 同一会话（账号会话列表只多一条而不是 18 条）

### v1.1.1

**面板体验升级**

- 修复：面板自动刷新不再吞掉正在填写的表单（检测到输入焦点或未提交内容时暂停轮询）
- 新增「上游模型」页：点刷新即用池内健康账号逐模型实测可用性与延迟（上游无模型清单端点，实测是最真实的对齐方式）
- Token 用量新增总计模块：总 token / 总输入 / 总输出 / 总请求与错误
- 用量统计修复重启清零：`usage.json` 现在启动时回读续账（此前只写不读）
- 网关设置页全字段中文化（开关改为「开启/关闭」）
- 下游密钥默认掩码，点「明文」按需查看完整密钥（再次点击恢复掩码）

### v1.1.0

**双凭证 + 一键收号**

- 账号池支持两种登录态：桌面端 `ot_session`（OAuth）与浏览器控制台态 `uin/skey`（登录 OrcaTerm Web 版后 F12 执行 `copy(document.cookie)` 即得），可混池运行
- 管理面板新增「一键收号」：生成 15 分钟一次性配对码与配套书签脚本，在已登录 Web 版的页面点书签即自动收号落盘
- 收号前网关先调上游只读接口实测凭据，坏票拒收不入库；配对码先消费后验证，天然防重放
- 控制台态票据失效（上游返回 `NEED_UIN_AND_SKEY`）与桌面态 `code=10050000` 同等处理：自动停用 + 跨号重试
- 面板账号列表按凭证形态显示（JWT 过期时间 / 控制台态标记）

### v1.0.1

**安全**

- 未设置面板密码时，管理面板与运维 API 只接受本机回环访问（此前：默认配置允许公网无鉴权访问）
- 移除 URL query 传面板密码的通道（密码只经请求头传递）
- `/debug/pprof` 纳入管理面板同级别鉴权（此前公网裸露）

**修复**

- SSE 流式响应被包装器缓冲、客户端长时间收不到增量（`statusRecorder` 未透传 `http.Flusher`，三套接口的流式全部受影响）
- HTTP/HTTPS 代理 CONNECT 后预读缓冲中的首包数据丢失（TLS 握手可能失败）
- 配置热重载后上游客户端未跟随新的 base-url / 超时 / 代理设置
- 429 在跨号重试耗尽后被误报为 502；登录态失效不再被误报为 500
- 登录态失效的换号重试此前因 401 不在 `retry-on` 列表而实际从不生效
- `tool_choice` 具名强制工具（`{"type":"tool"}`）此前被静默放行，与文档承诺不符

**其他**

- 账号池限速参数（max-in-flight / 冷却时长 / 重试策略）支持热重载
- 代理池快照按真实协议显示；用量落盘统一到 `state/` 目录
- 新增 `internal/server`、`internal/relay` 回归测试

## 注意事项

- 上游 `lightai.cloud.tencent.com` 的 AI 能力与模型目录可能随时变化；`GET /v1/models` 为静态快照，不会自动同步
- 上游不返回真实 token 数，网关用量为本地估算，仅供趋势参考
- 登录态（`ot_session` JWT）过期后上游无刷新接口，必须人工重新复制 Cookie
- 工具调用依赖 OrcaTerm 原生工具表（服务端下发），自定义 function 无法注册

## 免责声明

本项目仅供个人学习与研究使用。请仅使用本人持有且已获授权的 OrcaTerm 账号，遵守 OrcaTerm 服务条款与所在地法律法规；因使用本项目产生的账号风险与后果由使用者自行承担。

MIT License
