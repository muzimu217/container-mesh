# container-mesh 开发与平台 KPI 看板

更新时间：2026-11-14 10:30Z。分支：`develop`。远端 tip：`24bbf2f`。

这张表把代码、可运行产物、安装包、真机验收分开记账。交叉编译通过不等于平台交付完成；设计方案存在也不等于安装包完成。

## 产品与 M4

| 领域 | 当前状态 | 证据 / 下一步 |
|---|---|---|
| 真实锚点面板 `/panel` | **已完成，已在远端 tip 验收** | React + Vite + Three.js 构建产物由 Go 内嵌；令牌头鉴权；离线成员保留；`/panel-lite` 保留 |
| 面板白屏修复 | **本地已完成，待放行** | `/panel` → `308 /panel/`，`Cache-Control: no-store`；资源按浏览器 URL 解析测试 |
| 假设备 / 假遥测清除 | **本地已完成，待放行** | 删除香港假节点、假拓扑、假 CPU/RAM/SLA/流量、假动作成功回显；未知数据显示「未采集」 |
| 委派成员可拨 B-1 | **已完成并在 `24bbf2f` 验收** | oh-no 双向测试；10/10；本地另有凭证设备主动 ping/msg 验证 |
| 凭证设备后续命令 B-3 | **本地已完成，待放行** | `discovery.json` 保存派生字段；`join --cred` 后 `mesh ping` 5/5、`msg` delivered 1 |
| `--endpoint :0` B-2 | **已完成并在 `24bbf2f` 验收** | 绑定端口替换纯函数 + 回归测试 |
| 端口自检 | **已完成** | TCP/UDP 同端口提示、回环绑定提示、可执行动作 |
| 安装器 / 发布组装 | **已完成代码，真实 release 尚未验收** | `packaging/install.sh`、`tools/cmesh-install`、`tools/mkrelease`；需真实 GitHub Release 资产验收 |
| 中继路由 / 增量视图 | **未开始，推迟 v1.1** | M4-PLAN C 组；不混入当前 v1.0 收口 |

## 本地新增开发 KPI

| 任务 | 状态 | 验收 |
|---|---|---|
| `cmesh status` | **本地完成，完整门禁通过，待放行** | 注册命令；输出 account、roster version、online/total、成员 ID、端点、`last_seen`；JSON 稳定；离线成员保留 |
| `cmesh doctor` | **本地完成，完整门禁通过，待放行** | 复用 `cmesh probe` 的能力报告与退出码，避免第二套漂移实现 |
| 撤销后切断活跃连接 | **未开始，需先定连接事件接口** | 成员撤销后旧连接关闭、新握手拒绝、普通版本 bump 不误杀 |
| 优雅 LEAVE | **未开始，依赖连接生命周期接口** | 正常退出 ≤3s；kill -9 保持 ≤10s 兜底；迟到 LEAVE 不误杀重连成员 |
| 组网直连断点续传 | **未开始，独立协议任务** | 中断后 offset+hash 恢复；源文件变化、坏 `.part`、覆盖保护、空文件 |
| 远端 `@cat` 大输出 | **本地已修复，待放行** | QUIC ACK reader 不再用 stream 级 `io.LimitReader(4097)`；新增 4 KiB+真实 run frame/exit frame 测试，修复前稳定 `unexpected EOF` |
| 纯出站 NAT 反向直拨 | **已复现，归类为 v1.1 中继缺口** | netns 实测 client→anchor 10/10；anchor→client 因无 DNAT/无中继失败，不归因于凭证入网；需要中继路由/路径回退 |

## 平台包装 KPI

| 平台 / 形态 | 设计 | 可构建 | 可运行 | 可安装 / 签名 | 真机验收 |
|---|---:|---:|---:|---:|---:|
| Linux headless CLI / relay | ✅ | ✅ | ✅ | ⚠️ 手工安装器 | ⚠️ 干净容器/VPS矩阵待做 |
| Linux desktop Wails | ✅ | ❌ | ❌ | ❌ | ❌ |
| Windows CLI | ✅ | ✅ amd64 | ⚠️ 需 Windows 实机 | ❌ MSI/签名 | ❌ |
| Windows desktop Wails | ✅ | ❌ | ❌ | ❌ | ❌ |
| macOS CLI | ✅ | ✅ amd64/arm64 | ⚠️ 本机 CLI 可跑 | ❌ `.app`/签名/公证 | ❌ |
| macOS desktop Wails | ✅ | ❌ | ❌ | ❌ | ❌ |
| Android | ✅ gomobile 方案 | ❌ | ❌ | ❌ APK | ❌ |
| iOS | ✅ gomobile 方案 | ❌ | ❌ | ❌ IPA/签名 | ❌ |
| NAS | ⚠️ Linux 能力可复用 | ⚠️ amd64/arm64 | ⚠️ Docker/CLI | ❌ 厂商包/升级 | ❌ |
| router/OpenWrt | ⚠️ 设计方向 | ❌ MIPS/ARMv7/procd | ❌ | ❌ | ❌ |

## 当前门禁证据

最近一次本地修复批次已通过：

- `gofmt -l internal/ cmd/ tools/`：无输出
- `go vet ./...`
- `go build ./...`
- `go test ./... -race -count=1`
- `bash tests/run-tests.sh --quick`：41/41
- Linux amd64/arm64、Windows amd64 交叉编译
- 真实锚点面板：`/panel` 资源可达、令牌鉴权、假数据归零

`status/doctor` 本地增量已经通过 `go test ./internal/cli/ -run 'Status|Doctor' -count=1` 和 `go build ./...`；本地最终批次已再跑 `go test ./...`、`go test ./... -race -count=1` 与 `bash tests/run-tests.sh --quick`（41/41），全部通过。

## 并行开发队列

1. **status/doctor**：本地已实现，先完成完整门禁。
2. **LEAVE + 撤销切断**：共享 discovery/mesh 连接事件接口，先设计后并行实现，避免两个代理改同一路径。
3. **组网断点续传**：独立 loopback QUIC 故障注入，使用 `.part` + offset + hash，避免与 LEAVE 任务冲突。
4. **平台宿主**：先 Linux headless 安装闭环；再桌面最小 Wails 宿主；之后 Android/iOS 最小 gomobile binding + 真机单链路。
5. **v1.1 中继 / 增量视图**：单独预研，不阻塞当前 v1.0 收口。

## 外部等待

- `oh-no-agent` / `doubao-cloud-vm-01`：新 SHA 后复测 B-3、面板白屏、A1-A5。
- `kuku-sandbox-01` / `netns-probe`：已超过 30 小时静默，最终通知已发；截止后按失联结案并永久改派。
- 网主：交互会话放行当前 worktree 的提交与 develop 推送；本值守会话不 commit/push/merge。
