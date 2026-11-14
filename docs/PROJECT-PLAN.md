# container-mesh 项目总规划与交付口径

更新时间：2026-11-14。当前分支：`develop`。远端 tip：`24bbf2f`。

## 项目状态口径

项目分成四个交付层级，每层单独记账：

1. **代码实现**：功能是否在源码中完成。
2. **本地验证**：完整门禁和真实本地/回环环境是否通过。
3. **远端可复测**：是否已经进入 `origin/develop`，外部 agent 能否拉到同一 SHA。
4. **平台验收**：是否在目标系统安装、运行、入网、传输、退出和恢复。

只有第 3 层之后的结果才派给远程 agent 复测。交叉编译只证明目标二进制可以生成，不等于平台安装包或真机验收完成。

## 当前已完成

### M4-A：统一凭证与一键入网

- 凭证公开半与委派记录统一为同一结构和同一签名。
- `cmesh cred create` 生成带有效期、发现地址和设备额度的凭证。
- `cmesh join --cred <串>` 自动创建身份、本机签委派证书、先准入再公告。
- 委派证书在快照中依靠委派记录验签。
- B-1：委派成员可拨，外部实测双向 10/10。
- B-2：`--endpoint host:0` 解析为实际监听端口。
- B-3：凭证设备入网后的 `mesh ping/msg` 配置读取已修复；本地真实二进制验证 ping 5/5、msg delivered 1。
- e2e 已纳入第 11 节，当前门禁 41/41。

远端 `24bbf2f` 已包含 B-1/B-2 和基础凭证实现；B-3 最终修复仍在本地 worktree，需放行后推送。

### M4-B：端口自检与真实驾驶舱

- 锚点启动输出 TCP/UDP 同端口自检和可执行防火墙动作。
- `/api/mesh` 真实名册接口，面板令牌只认 `X-Panel-Token`。
- `/panel-lite` 轻量页面。
- `/panel` React + Vite + Three.js 驾驶舱由 Go `embed` 提供。
- 页面包含 3D 名册、在线/离线/端点/传输四维状态、过期快照和真实错误状态。
- 已清理原型中的五态演示场景、模拟按钮、两台香港假设备、假遥测、假拓扑和浏览器伪造在线状态。
- `/panel` 无尾斜杠白屏修复为 `308 → /panel/`，重定向 `no-store`。
- 生产面板动作统一改为只读提示；真实操作使用 CLI。
- 设计规范页中的示例表已标记为示意，假版本号 `v42/v43` 已清除；`spec-state-matrix-06` 用于 DOM 作用域验收。

当前面板本地构建已通过；新的 JS/CSS 构建资产已同步到 `internal/relay/cockpit-dist/`，仍待放行推送。

### M4-D：二进制、安装器与 README

- Go 交叉编译和 `tools/mkrelease` 组装器已完成。
- `latest` 别名、归档和 `SHA256SUMS` 逻辑已完成。
- `packaging/install.sh` 和 `tools/cmesh-install` 已入本地工作树。
- README 命令级错误已修正：口令旗标、参数顺序、凭证路径、relay 参数和发布件数字。
- 真实 GitHub Release 资产、签名、升级、卸载仍未验收。

### 本地新增运维线

- `cmesh status`：读取已验证 snapshot，输出成员、版本、在线状态、端点和 `last_seen`。
- `cmesh doctor`：复用现有 probe 能力与退出码。
- `@cat` 大输出：修复 ACK reader 的 4 KiB stream 截断，新增长输出 QUIC 回归测试。

## 当前未完成

### M4-C / v1.1：中继路由与增量视图

尚未开始。包括中继选路、路径切换、全受限 S3 自动可达、增量快照和慢消费者协议优化。纯出站 NAT 已实测：客户端主动拨锚点可用，锚点反拨纯出站客户端失败，这是当前中继/路径回退缺口，不能记为凭证缺陷。

### 连接生命周期

- 显式 LEAVE 尚未实现。
- 撤销后关闭已建立 direct QUIC 连接尚未实现。
- 当前可阻止新握手，异常断连有快速 offline 路径和 heartbeat 兜底。

### 组网直连断点续传

relay 配对通道已有恢复能力；组网直连的 `.part`、offset、hash 协议尚未实现。

## 平台包装路线

| 平台/形态 | 当前状态 | 下一验收 |
|---|---|---|
| Linux headless | 核心 CLI/relay 可运行，静态二进制可生成 | systemd、服务用户、日志、卸载、升级回滚、重启恢复 |
| Windows CLI | amd64 交叉编译通过 | Windows 实机入网、传输、退出、休眠恢复 |
| macOS CLI | amd64/arm64 交叉编译通过 | 本机链路、`.app`、签名、公证、钥匙串 |
| Linux desktop | 仅方案 | 选定发行版、Wails 宿主、WebKitGTK 依赖、安装包 |
| Windows desktop | 仅方案 | Wails + WebView2、MSI/卸载、托盘、签名 |
| macOS desktop | 仅方案 | Wails `.app`、文件权限、签名、公证、休眠恢复 |
| Android | 仅 gomobile 方案 | 最小 binding、文件 URI、前台服务、真机一条链路 |
| iOS | 仅 gomobile 方案 | 最小 binding、沙箱/Keychain、真机一条链路 |
| NAS | Linux/容器能力可复用 | 目标厂商矩阵、持久化、服务编排、升级回滚 |
| router/OpenWrt | 设计方向 | target/架构、BusyBox/procd、资源和断电恢复 |

## 里程碑

### M4：当前收口

目标：统一凭证 + 一键入网、端口自检 + 驾驶舱、二进制 + 安装器 + README。

完成判定：

- 代码实现和本地门禁全绿。
- 修复批次进入 develop。
- 外部 agent 在新 SHA 上复测 B-3、面板 308、真实名册、假数据归零。
- 发布件在真实 Release 资产上完成 checksum、安装、升级和回滚验证。

### M5：多网与可靠传输

候选项：一机多网、多租锚点、组网直连断点续传、LEAVE、撤销会话切断、权限和后台生命周期完善。

### M6：平台与生产化

候选项：Wails 桌面首包、Android/iOS 最小宿主、NAS/router 交付矩阵、签名分发、升级回滚、真实 WAN/NAT 生产验收。

## 每日 KPI 口径

每天更新 `docs/project-dashboard.html`，至少包含：

- 当日开发完成项与未完成项。
- 推送到 develop 的 commit 数量；未推送工作树单独列出。
- 完整门禁结果：fmt、vet、build、test、race、e2e、交叉编译。
- 各平台包装状态，不把方案、交叉编译、可运行、可安装、签名、真机验收混成一个“完成”。
- 外部 agent 的最后报告时间、当前任务和等待原因。
- 当日缺口与具体补做命令。

每日 17 点值守自动化必须先更新 HTML 看板，再输出 KPI 汇报。HTML 是日常查看入口，本文是路线和完成判定的文字正典。
