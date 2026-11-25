# CPA 二开基线

本目录是 CLIProxyAPI 的 Windows 发布目录，不是上游源码目录。源码、测试和构建应放在独立目录；不要在运行目录直接编辑 `static/management.html`，也不要把运行凭据复制到开发实例或测试样例中。

## 获取同版本源码

| 项目 | 标签 | 已核对的提交 | 技术栈 |
| --- | --- | --- | --- |
| [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI/tree/v8.1.2) | `v8.1.2` | `85225381d89915595598ed13766cd79890c03a84` | Go、Gin |
| [管理端](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/tree/v1.29.2) | `v1.29.2` | `3866a16878b04c19627ef0db06a33f5cd79878cf` | React、TypeScript、Vite、SCSS Modules、Bun |

在 CPA 运行目录之外的开发目录执行。下面的命令不会启动或修改现有 CPA 实例：

```powershell
git clone --depth 1 --branch v8.1.2 https://github.com/router-for-me/CLIProxyAPI.git cpa-backend
if ($LASTEXITCODE -ne 0) { throw 'CPA backend clone failed' }
git clone --depth 1 --branch v1.29.2 https://github.com/router-for-me/Cli-Proxy-API-Management-Center.git cpa-panel
if ($LASTEXITCODE -ne 0) { throw 'CPA panel clone failed' }
if ((git -C cpa-backend rev-parse HEAD) -ne '85225381d89915595598ed13766cd79890c03a84') { throw 'Unexpected CPA backend revision' }
if ((git -C cpa-panel rev-parse HEAD) -ne '3866a16878b04c19627ef0db06a33f5cd79878cf') { throw 'Unexpected CPA panel revision' }
```

后端 [CI](https://github.com/router-for-me/CLIProxyAPI/blob/v8.1.2/.github/workflows/pr-test-build.yml) 从 `./cmd/server` 构建；管理端 [CI](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/v1.29.2/.github/workflows/ci.yml) 执行 `bun install --frozen-lockfile` 和 `bun run verify`，后者包含测试、Lint 和构建。[发布流水线](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/v1.29.2/.github/workflows/release.yml) 将生成的 `dist/index.html` 重命名为 `dist/management.html`。部署自定义面板时应在独立实例验证 `management.disable-auto-update-panel`，避免后台更新覆盖自定义产物；现有实例的配置保持不变。

## 已有能力与接入点

| 能力 | CPA 当前实现 | 二开时复用 |
| --- | --- | --- |
| 批量导入已有凭据 | [v8 管理路由](https://github.com/router-for-me/CLIProxyAPI/blob/v8.1.2/internal/api/server_management_v8.go) 的 `POST /v8/management/credentials` 接收多个 JSON 文件，允许部分成功；[上传处理器](https://github.com/router-for-me/CLIProxyAPI/blob/v8.1.2/internal/api/handlers/management/auth_files_crud.go#L56-L114) 按文件处理 | 复用管理端上传客户端。批次应分别显示成功和失败；窗口内预检重名，不默认替换已有凭据 |
| 官方授权 | [OAuth 页面](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/v1.29.2/src/features/oauth/OAuthPage.tsx) 按提供商维护授权状态，[流程钩子](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/v1.29.2/src/features/oauth/hooks/useOAuthFlows.ts) 查询现有官方 OAuth 会话 | 浏览器交互交给官方 OAuth；授权无须新建账号，导入已有凭据也无须内嵌浏览器 |
| 自动日志更新 | [日志钩子](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/v1.29.2/src/features/logs/hooks/useLogStream.ts) 实现游标增量读取、断开后作废旧请求以及失败退避；[后端日志接口](https://github.com/router-for-me/CLIProxyAPI/blob/v8.1.2/internal/api/handlers/management/logs.go) 是全局日志并依赖文件日志设置 | 独立的开通任务事件不从全局日志解析；复用游标、重连与过期响应保护的设计 |
| 视觉和认证 | [主布局](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/v1.29.2/src/components/layout/MainLayout.tsx) 与[主路由](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/v1.29.2/src/router/MainRoutes.tsx) 管理侧栏和页面；[管理客户端](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/v1.29.2/src/services/api/client.ts) 管理认证和连接版本 | 复用导航、主题、通知和按连接隔离请求的机制；任务状态接口沿用管理鉴权 |

## 实现范围与验收

CPA 现有接口只导入或授权已有账号，不创建外部平台账号。自有系统或企业官方开通接口确认后，账号开通任务可以按渠道隔离权限、队列及状态，逐条记录「排队、校验、开通、写入凭据、完成／失败」。日志只储存阶段和脱敏原因，不储存密码、令牌、回调参数或上游原始响应。

- **渠道隔离**：一个渠道失败不影响另一渠道，切换渠道或 CPA 连接后不显示旧请求结果。
- **进度**：自动更新当前阶段、已完成数、失败数及卡点；断线后从上次事件位置续读。
- **核心路径**：官方开通 API 和已有凭据导入不依赖浏览器；需要人工确认的官方 OAuth 单独显示等待状态，不把等待当成失败。
- **部分失败**：成功项保留。对明确可重试的失败项才重试；对结果不确定的开通请求先查询官方状态，避免重复创建。

当前交付仅包含源码核对和 Git 基线，不含已实现或已上线的开通模块；未访问代理网关，也未调用生产账号接口。
