# CPA 多渠道注册机

> 此初版交付包已被 `../artifacts-key5-fix/` 的修复版取代，请勿重新部署初版二进制。初版误取 Dashboard 的 28 位缩略 key，且 `/models` 为公开接口，原“可用性校验”结论不成立。修复版读取复制按钮的完整 46 位值，完成基础 $5 引导后用真实对话请求验证；指定账户第 5 个 key 已修正。历史执行数据保留以便追溯。

基线：CLIProxyAPI v8.1.2、管理端 v1.29.2；本机验证日期：2026-11-25。

## 使用

打开本机 `http://127.0.0.1:8317/management.html#/registerbot`，选择渠道，按每行 `账户|密码` 输入，点击开始批量注册。页面实时显示阶段、重试、降级和写回结果。可暂停、恢复、重试、取消，或在需要人工验证时继续原浏览器会话。

渠道配置支持独立队列、并发数量、间隔、重试次数、目标 key 数、固定节点和人工模式。当前首个驱动为 FreeAIAPIKey；可创建多个独立渠道配置。新增站点驱动的接口和约束见后端 `internal/registerbot/README.md`。

网关默认使用 `http://127.0.0.1:12450`，Clash/sing-box 控制面为 `http://127.0.0.1:12440`。本机实跑渠道固定到现有香港 IEPL 02 节点。单一代理端口的共享选择器只能在任务期间串行持有租约，不能提供同时独立的多个出口，也不承诺免除服务方封禁。

Chrome 由进程池管理，每账户使用独立 BrowserContext。降级路径为 `embedded → attach → system → manual`；网关为 `controller → proxy_only → disabled`。降级和卡点以 SSE 事件报告。验证码、二次验证或额外身份检查保持人工接续。

## 已完成的本机验收

- 指定账户完成 Google 登录、dashboard key 获取、模型列表验证与 FreeAi 写回。
- FreeAi key 数量从 **4 增至 5**，新增 key 提示为 `sk-…-m0E`。
- 新 key 的 `proxy-url` 为 `http://127.0.0.1:12450`；独立回读后再次经代理请求模型列表，返回 **15 个模型**。
- 写回复用 `providerKeyImport` 的 v8 GET/PUT 配置通道，保留 ETag/If-Match、文件 CAS、配置验证和热更新；成功后再次回读确认。
- 实际部署文件与构建产物哈希一致。已验证 Chrome 受管实例在后端重启后复用。
- SSE 未认证请求返回 401；游标重放仅返回后续事件，事件顺序与保留历史尾部一致。
- 桌面 1280px 与手机 390px 页面检查通过，无横向溢出、无页面运行时错误。

Google Workspace speedbump 的“我了解”分支已实现并通过 DOM 夹具验证；本次成功账户未出现该分支，未将其计为真实站点实测。

## 测试情况

- 注册机、管理处理器与 API 路由回归通过；`go build ./cmd/server` 通过。
- `bun run verify`：**2405 通过，0 失败**，包括 ESLint、TypeScript 和单文件构建。实际本机 Bun 为 1.3.14，仓库声明版本为 1.4.3。
- 全仓库 Go 测试最初有 13 个既有失败，其中 4 个 Windows 清理顺序问题通过最小测试修复解决。最终全量执行有 **10 个失败、分布在 5 个包**，包括另一个间歇性 quota recovery 失败；这 10 项均已在未修改的 HEAD 归档源码复现。失败涉及 TUI、Windows Git 语义、executor 流式处理和 translator 行为。完整执行记录与最终结果保存在 `verification` 目录，不能将全量测试表述为通过。
- Go race 检测未执行成功：本机 `CGO_ENABLED=0`，未发现可用 C 编译器。

## 部署与文件

当前运行目录已部署完成。再次部署可执行：

```powershell
& 'E:\LY\Tool\CPA\cpa-dev\artifacts-registerbot\deploy.ps1'
```

脚本先备份当前后端、面板和配置，随后只部署后端与面板。备份位于运行目录下 `cpa-dev/deployment-backups/`，不包含在发布包或 Git 提交中。自定义面板自动更新已禁用。

运行状态位于 `E:\LY\Tool\CPA\registerbot\`：队列使用 AES-GCM 加密，Windows 密钥由当前用户 DPAPI 保护。中断任务在重启后暂停，已捕获 key 优先续写。取消任务会清除该任务密码，取消后不能恢复；需要重新提交账户。浏览器人工会话不会跨重启保留。运行凭据、配置和 Chrome profile 均不打包、不提交。

`verify-panel.cjs`、`verify-stream.cjs` 为本机验证脚本；`start-live-run.ps1` 从进程环境变量 `CPA_REGISTERBOT_ACCOUNT` 读取测试账户，不在脚本内保存真实凭据。验证摘要见 `verification/e2e-success.json`、`verification/panel-browser.json`、`verification/sse-live.json`。
