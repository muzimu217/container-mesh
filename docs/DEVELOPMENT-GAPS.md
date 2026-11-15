# 开发整改清单：产品、实现、文档与验收缺口

> 本文是面向开发者、测试者和文档维护者的整改 brief。它记录当前审阅发现的缺口、影响、完成判据和优先级。
>
> 本文不替代 [`REQUIREMENTS.md`](REQUIREMENTS.md) 的需求正典，也不把方案稿当作已交付能力。所有整改完成情况必须以固定 SHA、可复现命令和实际环境证据为准。
>
> 审阅依据：源码、源码注释、`README.md`、`docs/REQUIREMENTS.md`、`docs/DESIGN.md`、`docs/architecture/`、`docs/PROJECT-PLAN.md`、`docs/DEV-KPI.md`、`docs/ROUND-LOG.md`、`deploy/` 和 `packaging/`。当前工作树存在未提交变更，因此本清单不指定一个未经确认的验收 SHA。

## 1. 总体判定

项目已经具备较清楚的产品定义和较完整的设计体系，身份、发现、成员认证、直连通信、消息、文件、只读诊断、端口转发和锚点面板已有不同程度的实现或验证。

当前影响项目对外可用性和开发协作效率的缺口集中在四类：

1. **纯出站场景还缺少真正的 Mesh 中继兜底。** 旧阶段 1 relay 是共享 code 的 A/B 配对通道，不能证明 device-ID Mesh relay。
2. **连接生命周期和可靠传输还没有闭环。** LEAVE、撤销切断既有会话、组网直连断点续传仍未完成。
3. **真实环境和发布交付证据不足。** 公网/WSS/443/代理/NAT、Linux systemd、升级回滚、重启恢复和平台安装尚未形成完整验收链。
4. **文档状态已经分叉。** 多份文档使用不同的阶段、SHA、日期和完成口径，开发者无法从单一入口判断当前事实。

在以下 P0 缺口完成前，不应对外宣称“完整 Mesh”“纯出站设备始终可达”或“生产级发布已完成”。

## 2. 优先级和完成定义

- **P0：阻塞核心产品闭环。** 不完成就不能称为当前产品的核心能力已经交付。
- **P1：阻塞生产化或扩大用户范围。** 核心链路可以运行，但不能稳定交付给更多用户。
- **P2：后续增强。** 不得提前混入当前核心范围，也不能用方案文档替代实现证据。

每项整改必须分别记录四层状态：

```text
设计存在 → 代码完成 → 本地验证通过 → 真实环境/目标平台验证通过
```

“交叉编译通过”只能记为可构建；“单元测试通过”不能替代真实进程、真实网络或真实平台验收。

## 3. P0：补齐 Mesh 中继与纯出站可达

### 3.1 实现 device-ID relay trunk

**问题**

当前 `mesh ping`、`msg`、`send-to`、`bench` 主要使用 direct QUIC。旧 `send/recv` relay 使用共享 code 和 A/B room 配对，缺少 Mesh 所需的成员认证、目标路由和中继路径语义。真实 netns 复测已经证明：纯出站 client 可以主动连接锚点，但锚点反向连接该 client 失败。

**对应范围**

- `REQUIREMENTS.md`：R-3.2、R-3.5、R-3.6、C-3、C-5、N-9
- `PROJECT-PLAN.md`：M4-C / v1.1
- `DEV-KPI.md`：mesh relay-only trunk
- 设计依据：`DESIGN.md`、`architecture/MESH-DESIGN.md` 的连接、路由和 E2E 章节

**需要完成**

- 建立基于 device ID 的 relay trunk，而不是复用 code/A-B 配对语义。
- 中继双方先完成成员身份验证，再打开目标设备的逻辑路径。
- 实现 direct-first：先尝试直连，失败后自动选择可用中继。
- 实现中继节点的流数、吞吐和并发配额。
- 实现路径失败后的候选节点切换和退避。
- 让 `ping`、`msg`、`send-to`、`run`、`forward` 使用统一的路径选择接口。
- 记录实际路径类型：`direct`、`punched`、`relay-only`、`unknown`；未取得证据时不得猜测。
- 对中继数据面加入端到端加密，使中继只能看到必要的下一跳和流量元数据，不能看到业务明文。

**完成判据**

- 两个无公网入站端口的真实节点可以通过第三个可达节点完成 `mesh ping`、`msg` 和文件传输。
- 直连可用时不会无故经过中继；阻断直连后会自动切换到中继。
- 中继恢复或直连恢复时，路径切换行为有明确协议和测试断言。
- 中继抓包不能得到消息、文件或诊断内容的明文。
- 中继超过流数或带宽配额时只拒绝新的中继请求，不影响自身业务。
- 测试至少覆盖：成员伪造、目标伪造、环路、TTL、路径切换、慢消费者、并发关闭和中继节点重启。
- 在 Linux netns/SNAT 以及授权的真实公网/WSS/443 环境中完成一次真实进程验收，并保存命令、拓扑、SHA、样本和结果。

## 4. P0：补齐连接生命周期和撤销语义

### 4.1 显式 LEAVE

**问题**

当前异常断连有较快的 offline 路径和 heartbeat 兜底，但主动退出没有独立的 LEAVE 语义。需求要求优雅下线在 3 秒内收敛，不能把正常退出完全当作异常断线。

**需要完成**

- 设计并实现带设备 ID、成员视图版本和连接代数的 LEAVE 事件。
- 发现节点收到 LEAVE 后只下线对应的当前连接，不误杀重连后的新连接。
- 对迟到 LEAVE、重复 LEAVE、旧连接 LEAVE 和进程崩溃分别处理。

**完成判据**

- 正常退出后其他成员在 3 秒内更新为 offline。
- `kill -9`、断网和容器网络销毁仍在 10 秒内收敛。
- 重连竞态测试证明旧 LEAVE 不会把新连接标记为 offline。

### 4.2 撤销后切断既有会话

**问题**

当前成员移除可以阻止新握手，但现有 direct QUIC 会话尚未按需求在成员撤销后主动销毁。部署指南中的“撤销即失效”表述超过了当前实现。

**需要完成**

- 将成员视图更新与活跃会话注册表连接起来。
- 收到确认的撤销快照后，关闭被撤销设备的所有活跃会话、流和路径记录。
- 普通成员列表版本增长不能误杀未被撤销的会话。
- 处理发现节点分区、重复快照、旧快照和撤销后重连。

**完成判据**

- 撤销广播后，一个推送往返内旧会话被关闭；测试允许的宽限不超过需求规定的 30 秒。
- 被撤销设备的新握手被拒绝。
- 其他成员的现有会话不受无关版本增长影响。
- 覆盖 direct、relay、消息、文件和 forward 流的回归测试。

## 5. P0：补齐组网直连断点续传

**问题**

阶段 1 relay 配对通道已有部分恢复能力，但 Mesh 的 `send-to` 直连文件传输尚未具备 `.part`、offset、hash 协商和源文件变化保护。

**需要完成**

- 设计 Mesh 文件传输的传输 ID、文件元数据、源文件指纹、offset 和 hash 协议。
- 接收方使用临时 `.part` 文件，完成后原子改名。
- 处理中断、重复发送、坏 `.part`、源文件变化、空文件和目标文件已存在。
- 直连和 relay trunk 共享业务层恢复语义，不能各自实现一套不兼容协议。

**完成判据**

- 在真实进程中于传输中断后恢复，接收方从正确 offset 继续。
- SHA-256 一致，坏前缀和源文件变化会明确失败或重新开始。
- 不会把未完成文件伪装成完整文件，也不会无提示覆盖已有文件。
- 覆盖小文件、空文件、大文件、并发传输和重启恢复测试。

## 6. P0：建立单一项目状态基线

**问题**

当前文档存在以下冲突：

- `PM-REVIEW.md` 与 `PROJECT-PLAN.md` 使用不同远端 SHA；
- 部分文档时间晚于当前会话日期；
- 计划写本地待放行 18 项，但工作树实际未提交条目更多；
- 阶段 2 验收仍写“未开始”，同时需求表和轮次日志已经记录多项实现；
- `docs/README.md`、`TEAM.md`、`M4-PLAN.md`、`MESH-NET.md` 保留早期阶段状态；
- 测试包数量和资源 KPI存在不同口径。

**需要完成**

建立一份唯一的当前状态矩阵，至少包含：

| 字段 | 内容 |
|---|---|
| 需求编号 | R/P/C/O/N 编号 |
| 功能 | 用户可理解的能力名称 |
| 设计位置 | 设计文档和章节 |
| 代码位置 | 包、文件和关键入口 |
| 代码状态 | 未开始/部分/完成 |
| 本地测试 | 命令、日期、结果 |
| 真实环境 | 环境、拓扑、SHA、结果 |
| 发布状态 | 未发布/候选/已发布 |
| 当前缺口 | 具体下一步 |
| 负责人和目标日期 | 明确到人和日期 |

同时统一：

- 一个候选 SHA；
- 一个截至时间和时区；
- 一个工作树变更统计口径；
- 一个测试包统计命令；
- 一个资源指标定义和采样方法。

**完成判据**

- `docs/README.md`、`PROJECT-PLAN.md`、`DEV-KPI.md`、dashboard、GPM、PM Review 引用同一个状态基线。
- 需求中的“阶段 2 验收”改为“阶段 2整体未收口”，并逐项标注 `PASS / PARTIAL / FAIL / NOT MEASURED / NOT IMPLEMENTED`。
- 开发者只需从文档索引进入一张矩阵，就能判断某项能力是否可以对外使用。
- 所有外部复测报告写明被测 SHA，不允许使用工作树未固化状态作为“已通过”证据。

### 6.1 当前候选 SHA 与历史文档冲突

**问题**

当前主工作树与 `origin/develop` 为 `1f64cdc`，文件级工作树统计为 41 条变更；`docs/PM-REVIEW.md` 仍引用旧的 `24bbf2f`，不能作为当前候选验收证据。历史 `ROUND-LOG` 保留当时的 41/41 记录是历史事实，但不能覆盖当前 worktree 的 38 shell + credential Go integration 口径。

**完成判据**

- 固化一个候选 SHA，重新跑全量门禁和 panel/CLI 黑盒。
- PM Review、PROJECT-PLAN、DEV-KPI、GPM、dashboard 使用同一候选 SHA 和同一工作树统计口径。
- 历史轮次只保留历史数字，新状态入口不引用旧批次作为当前证据。



### 7.1 标记或归档旧 SD-WAN 方案

`docs/architecture/ARCHITECTURE-SDWAN.md` 仍将 TUN、L3、SOCKS5、透明 `ping/ssh`、`cmesh up` 写成目标，但当前 R3 已明确不做自研 VPN、TUN、全局虚拟 IP和透明接管。

需要在文件开头增加醒目标记：

```text
历史提案 / 已被 R3 产品定义替代 / 不作为当前实现范围
```

同时在 `docs/README.md` 中把它放到历史研究或归档位置，避免新开发者把它当作当前技术正典。

### 7.2 修正部署指南和 README 的能力状态

`deploy/MESH-NET.md` 仍把按设备文件传输和端口转发写成未来能力，但当前 README 和源码已经提供对应命令。需要按当前实际能力重写“能用/还不能”清单，并明确：

- direct Mesh 文件传输和阶段 1 relay 配对文件传输不是同一条路径；
- Mesh 直连断点续传是否完成；
- Mesh relay-only 是否完成；
- 撤销是只拒绝新连接，还是也会关闭旧连接；
- 锚点自身是否支持主动 `ping/msg`，若不支持必须写明限制。

### 7.3 统一“零入站”和“无中心”表述

所有用户可见文档都必须明确：

- “零入站端口”描述的是普通客户端，锚点/发现节点仍需要可达端口；
- 当前没有中心数据转发，但控制面依赖自托管发现节点；
- 没有可达节点时不保证通信；
- relay-only 自动兜底完成前，纯出站节点不应宣传为始终可被反向访问。

### 7.4 收窄远程命令的说法

产品简报中的“互执行命令”容易被理解为任意 shell 执行。当前真实能力是内置只读动词：`@info`、`@disk`、`@mem`、`@cat`、`@ls`。

需要统一为“受认证的内置只读诊断”，并把任意命令执行明确标为未来的权限控制能力。文档、面板和示例不得暗示已经开放 shell。

### 7.6 其他过时入口

以下入口仍需要按当前状态矩阵逐项清理，不能直接当作当前能力说明：

- `docs/PM-REVIEW.md` 仍引用旧 tip `24bbf2f` 和旧 status/credential 状态。
- `docs/REQUIREMENTS.md` 的阶段 2/PR-M2、status、客户端 credential 接线和验收章节存在历史状态，必须标记历史快照或更新到当前候选 SHA。
- `packaging/README.md`、`packaging/start.sh` 和部分部署入口仍把旧 `/dashboard` 当主入口；代码中 `/dashboard`、`/status` 仍注册为阶段 1 legacy 端点，因此应标为 legacy/demo，而不是声称不存在。当前主产品入口是 `/panel`/`/panel-lite`。
- `docs/README.md` 仍保留旧 `/dashboard` 状态入口引用，需与当前 panel/status API 事实统一。
- `allowedDiscoveryHost` 对字面 IP 做公有可路由校验，但普通 hostname 目前只拒绝 `localhost` 并做语法放行；尚未看到 DNS 解析后再次拒绝私有/环回/保留地址的实际 egress enforcement。因此当前修复不能宣传为完整 DNS rebinding/解析后 SSRF 防护，需在实际拨号层补有超时、无凭据泄露的解析后校验。
**完成判据**：每个入口标注文档日期/候选 SHA；旧设计和历史轮次保留历史标记；用户可见命令与当前源码、测试和发布资产一致。

需求中“界面阶段 3后置”和 M4“锚点轻量面板已交付”并不矛盾，但必须写成明确规则：

- M4 的 `/panel` 和 `/panel-lite` 是锚点只读运维面板；
- 它们不等于桌面客户端；
- Windows/macOS/Linux 桌面 App、Android/iOS 客户端仍属于后续平台交付；
- 面板不持有设备私钥，也不自行充当 Mesh 节点。

## 8. P1：完成真实环境与发布验收

### 8.1 C-5 网络环境验收

需要在授权环境中验证：

- Linux 受限容器；
- 只能 HTTP(S)/WebSocket 出站的环境；
- WSS/443；
- 反向代理或 CDN 前置；
- 至少一个真实公网节点；
- NAT/SNAT 纯出站场景；
- 网络中断、代理断开和连接恢复。

每次实验必须记录：机器、系统、架构、网络拓扑、命令、样本数、吞吐、RTT、丢包、CPU、RSS、连接恢复结果和被测 SHA。loopback 数据只能作为本地上界，不能作为公网结论。

### 8.2 Linux headless 安装闭环

当前安装器主要完成下载、SHA-256 校验和二进制落盘，仍需真实 Debian/Ubuntu systemd 环境验收：

- 专用服务用户；
- systemd unit；
- 日志和 journald；
- 启动、停止、SIGTERM 优雅退出；
- 崩溃自动拉起；
- reboot 后恢复；
- 原子升级；
- 升级失败回滚；
- 卸载时保留或清除状态的明确选项；
- 安装后的 join/relay/send/recv smoke test。

`packaging/systemd/`、`packaging/install.sh` 和 `tools/cmesh-install` 的行为必须保持一致，README 的安装命令必须与真实 Release 资产名称一致。

### 8.3 Release 验收

必须在真实 Release 资产上完成：

- 固定下载地址；
- `SHA256SUMS` 覆盖全部发布文件；
- 下载后校验失败会中止；
- 干净环境安装；
- 升级和回滚；
- 卸载和状态保留；
- 版本化文件与 `latest` 别名一致。

在这些证据完成前，README 不应把发布页写成已经可用的正式安装入口。

### 8.4 systemd 示例不是可直接启用的生产服务

当前 `packaging/systemd/` 只有 unit/env/README 示例。unit 依赖预先存在的 `cmesh` 用户/组、状态目录、设备/账号身份、members/certs 和 account public key；README 没有完成这些初始化，也没有真实 `systemctl enable --now`、journal、权限降级、SIGTERM、Restart/reboot 验收。示例中的 `CMESH_PASSPHRASE` 只能由外部 secret/environment 注入，不能写真实值。

**完成判据**：Debian/Ubuntu systemd VM 中从干净状态执行 user/group/state/identity 初始化，`systemd-analyze verify`、enable/start、健康检查、journal、restart、kill 自动拉起、reboot 恢复、升级 rollback、send/recv smoke 全部有命令和退出码证据。

**安装安全补充**：

- unit 的 `CMESH_PASSPHRASE` 会由 `EnvironmentFile` 继承，但空值时 CLI 非交互启动可能因 stdin EOF 失败；README 必须说明 secret 注入前置。
- unit 省略 `--account-pub` 会使证书目录只做过期检查、不做账号签名验证，且不安装 credential enrollment handler；生产模板必须明确 account public key 和准入前置。
- shell/Go installer 仍需注入 rename/link 失败测试、并发锁、旧版本回滚和真实二进制启动校验；mkrelease 需固定 LICENSE 路径、校验输入版本/平台、验证归档内容，不能依赖当前 cwd。
### 8.5 installer 原子切换与跨实现一致性

shell 与 Go installer 都有版本目录和 symlink，但必须分别覆盖链接切换失败、中断窗口、旧 current 保留、Go/shell OS/arch/版本命名一致性。当前 macOS/BSD 分支和 Linux `mv -T` 行为不同，不能只靠单平台 fixture 测试宣称原子升级。

**完成判据**：注入写入/rename/link 失败，旧版本仍可执行；shell/Go 对相同 manifest 生成相同资产和路径；Linux amd64/arm64 真实安装 smoke 通过。

### 8.7 Release 输入和签名边界

`mkrelease` 当前可本地 staging、latest alias、tar.gz 和 SHA256SUMS，但仍需验证输入文件的版本/平台/架构、排除已有 latest alias 重复收集、归档可解包、LICENSE 不依赖当前 cwd、二进制可启动。SHA256SUMS 只提供清单一致性，不是独立发布者签名；正式 Release 仍需授权上传、下载、checksum、安装、升级、回滚和签名/provenance 方案。


当前正式需求 P-6 是 20 台设备 RSS 小于 100 MiB，KPI 又使用常驻 join 进程 15 MB 目标；两者没有统一编号、工作负载和采样方法。

需要先定义：

- 测量的是 VmRSS、峰值 RSS、heap 还是容器内存；
- 空闲、在线成员、发现流、direct QUIC、relay trunk、文件传输和 bench 分别使用什么工作负载；
- 采样窗口和采样间隔；
- 是否包含一次性峰值；
- Linux 与 macOS 的统计差异；
- 单节点和 20 节点的门槛。

然后把 15 MB 目标正式编号，或删除它并只保留 P-6。所有性能结论必须提供 A/B 同刻对照，不能把 macOS loopback 结果外推为公网性能。

重点补测：

- Mesh relay trunk 的 CPU、RSS、吞吐和流数；
- direct/relay 切换时的资源变化；
- Linux/WSS/443；
- 丢包、延迟、慢消费者和长时运行；
- 传输结束后的资源回落；
- 高并发关闭和重连期间的 goroutine、fd、内存回收。

## 10. P1：平台交付矩阵

当前平台设计已经存在，但“方案、可构建、可运行、可安装、已签名、真机验收”必须分开记账。

按以下顺序推进：

1. Linux headless 安装和恢复闭环；
2. Windows/macOS CLI 实机入网、传输、退出和休眠恢复；
3. Linux/Windows/macOS Wails 最小桌面宿主；
4. Android/iOS 最小 gomobile binding 和真机单链路；
5. NAS 厂商/架构矩阵；
6. OpenWrt/router 目标、资源和断电恢复。

每个平台必须记录系统版本、架构、安装包哈希、安装方式、权限、运行命令、测试结果和已知限制。交叉编译不能替代真机验收。

## 11. P1：补齐团队权限模型的设计边界

当前产品定案是“一个身份就是一个团队，成员设备全互信”。这对个人开发者简单有效，但小团队场景会默认授予过宽权限。

本阶段可以不立即实现完整 ACL，但必须先写清楚下一阶段的权限模型：

- 谁可以加入、批准和撤销设备；
- 谁可以发送文件和消息；
- 谁可以执行只读诊断；
- 谁可以使用 `forward`；
- 谁可以作为 relay；
- 谁可以访问哪些端口；
- 任意命令执行是否永远关闭、如何单独授权；
- 多网络或多身份如何隔离。

在权限模型明确前，不得把当前全互信模型宣传成企业级细粒度访问控制。

## 12. 开发流程要求

每个整改项必须遵守以下流程：

1. 先确认需求编号和设计章节；
2. 先写能证明缺陷的失败测试或验收脚本；
3. 实现修复；
4. 再次运行红转绿测试；
5. 跑完整本地门禁：

   ```bash
   gofmt -l .
   go vet ./...
   go build ./...
   go test ./... -race -count=1
   bash tests/run-tests.sh --quick
   GOOS=linux GOARCH=amd64 go build ./cmd/cm
   GOOS=linux GOARCH=arm64 go build ./cmd/cm
   ```

6. 在真实目标环境中验证需要平台或网络条件的项目；
7. 更新需求状态、设计状态、项目计划、KPI和轮次日志；
8. 外部复测使用同一固化 SHA；
9. 报告中明确区分已实现、已验证、已发布和用户可用；
10. 所有对外 Git 操作继续遵守项目的身份、DCO、本地全绿和口头放行规则。

## 13. 建议开发顺序

建议按下面顺序收口：

1. 固定基线 SHA，建立统一状态矩阵；
2. 修正文档中的历史状态和错误能力承诺；
3. 完成 device-ID relay trunk 和 direct-first fallback；
4. 完成端到端中继加密、配额和路径切换；
5. 完成 LEAVE 与撤销后切断活跃会话；
6. 完成 Mesh 文件断点续传；
7. 在 Linux netns、真实公网、WSS/443 和代理环境验收；
8. 完成 Linux headless 安装、升级、回滚和重启恢复；
9. 统一资源/性能指标并补齐对照实验；
10. 再推进桌面、移动、NAS 和 router 平台交付。

## 14. 对外发布前的最低门槛

满足以下条件后，才可以把当前版本描述为可对外交付的 Mesh 产品：

- 纯出站节点在直连失败时可以自动 relay-only 通信；
- `msg`、文件、诊断和 forward 使用同一可靠路径模型；
- 撤销后旧会话会按要求关闭；
- 组网文件支持中断恢复和完整性校验；
- Linux headless 安装、升级、回滚和重启恢复已在真实系统完成；
- C-5 的 WSS/443/代理环境已有真实证据；
- 文档、README、面板和发布说明使用同一 SHA 和同一状态矩阵；
- 所有性能数字都带环境、命令和样本，不把 loopback 结果写成公网承诺。

在此之前，建议对外使用“低权限设备 Mesh 原型/开发版”或“支持直连的自托管设备通信工具”等准确表述，并明确列出 relay-only、平台安装和真实 WAN 验收状态。
