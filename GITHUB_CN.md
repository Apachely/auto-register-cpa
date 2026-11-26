# CPA 二开仓库

GitHub：https://github.com/Apachely/auto-register-cpa

本项目原本由三个独立 Git 仓库组成。为保留已有提交历史与本机开发结构，同一 GitHub 仓库使用以下分支保存它们：

| 内容 | GitHub 分支 | 本机目录 |
| --- | --- | --- |
| 交付文件、运行脚本、补丁和验证记录 | `main`、`feat-ly/registerbot-delivery` | `E:\LY\Tool\CPA` |
| 完整 Go 后端源码 | `feat-ly/registerbot-backend` | `E:\LY\Tool\CPA\cpa-dev\cpa-backend` |
| 完整 React 管理端源码 | `feat-ly/registerbot-panel` | `E:\LY\Tool\CPA\cpa-dev\cpa-panel` |

`main` 不包含嵌套源码仓库的 Git 元数据。源码分支包含各自上游历史和本地二开提交，不能直接合并到交付分支。

## 获取完整项目

在新的空目录中执行：

```powershell
git clone --branch main https://github.com/Apachely/auto-register-cpa.git
Set-Location auto-register-cpa
git clone --single-branch --branch feat-ly/registerbot-backend https://github.com/Apachely/auto-register-cpa.git cpa-dev/cpa-backend
git clone --single-branch --branch feat-ly/registerbot-panel https://github.com/Apachely/auto-register-cpa.git cpa-dev/cpa-panel
```

现有本机源码仓库保留 `origin` 指向各自上游，新增 `github` 指向本仓库；后续二开提交推送到对应源码分支。交付目录的 `origin` 指向本仓库。

## 2026-11-26 提交状态

- 后端提交 `b45373ffd412d7b69a2c8ffed043b7e7802996d9`：固定路由共享并发、导入提交串行保护、区分问卷完成与欢迎额度、普通登录卡顿重试，以及相关回归测试。
- 管理端提交 `96d2f44`：保留已提交的批量导入、注册管理页面和事件顺序修复；本次没有未提交的前端代码。
- 交付目录包含此前未跟踪的 onboarding 源码副本、补丁、回滚工具和验证记录，并记录本机现有配置改动及三个 bot 启停脚本的删除。

提交前执行并通过：Go 格式检查、`git diff --check`、`go test ./internal/registerbot -count=1`、管理处理器的 RegisterBot 测试、`go vet`、后端构建、12 个 onboarding DOM 用例，以及 Google 输入/同意页面夹具检查。本次没有重跑全仓库 Go 测试、管理端全量检查或真实账号流程；之前的全量测试限制见历史交付记录。

`CPA-DEVELOPMENT.md` 是最初的二开基线，其“尚无开通模块”结论只描述当时状态。后续进度以源码提交和 `cpa-dev/artifacts-onboarding-completion/VERIFICATION.txt` 等验证记录为准。

## 本机文件与公开内容

管理密钥、客户端密钥、运行配置、浏览器资料、bot 代理配置、PID、运行日志和新生成的后端可执行文件不纳入本次提交。`registerbot/settings.yaml` 是已有的受版本控制设置，不含账号密码或 API key。

提交前检查了交付历史、二开提交和待发布文件，并与本机已知凭据比对。扫描命中的模拟测试 key 已逐项核实；完整后端中的其他命中均与公开上游基线一致。历史交付截图只展示模拟数据或遮蔽后的账号/key。

新机器需要自行配置 `config.yaml`、管理密钥、服务商凭据和代理环境。源码编译方式与约定分别见后端和管理端分支的 `AGENTS.md`；历史验证脚本包含本机绝对路径，不应直接用来启动新机器上的真实账号任务。
