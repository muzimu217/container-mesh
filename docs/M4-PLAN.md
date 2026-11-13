# M4 交付计划（v1.0 · 目标 2026-11-15）

> 本文是**执行计划**：每项写明落点文件、验收命令、完成判据。排期不是承诺，
> 完成判据才是承诺。网主指令：底层引擎先扎实，main 只在成品版本更新。
>
> **进度（2026-11-13）**：B1/B2（面板接口 + 页面）与 D（发布件 + 安装器）已落地
> 并本地验证；A1/A2（统一凭证 + 一键入网）完成了凭证格式与委派签发的实现与测试，
> 发现节点准入接线与客户端命令待做；C（中继路由 + 增量视图）未开工，按范围建议
> 推迟到 v1.1。

## 0. 范围建议（需网主一句确认）

M4 原有四组：(a) 统一凭证+一键入网、(b) 锚点端口自检+可视化面板、(c) 中继路由+
增量视图、(d) 二进制+安装脚本+README。

**建议 v1.0 = (a) + (b) + (d)，把 (c) 中继路由挪到 v1.1**。理由：

- (c) 是 R-3.2/R-3.3 的深层网络能力（选路、路径切换、增量视图 O-1），工作量与
  风险都远超其余三项，而 S1/S2 场景（至少一方有公网 IP）直连已可用；
- (a)(b)(d) 恰好是**网主做宣传时需要的东西**：一键入网、可看的面板、能下载安装；
- S3 全受限容器（必须走中继）仍可用锚点直连转发兜底，产品定位不受影响。

若网主要求 v1.0 必含中继路由，则 (a)(d) 需压缩到最小（凭证+安装脚本），面板
降级为只读静态页。请给一句口径，我按此排期。

## 1. (a) 统一凭证 + 一键入网

| 步 | 落点 | 验收 |
|---|---|---|
| A1 凭证格式与签发 | `internal/identity`：凭证 = 账号公钥 + 设备槽位 + 过期时间 + 账号签名 | 单测：过期/撤销/签名不符三类凭证全部被拒 |
| A1.5 委派签发（已落地） | `internal/credential` + `identity.Delegation` + `DeviceCert.SignerPub`：账号私钥不出门，委派私钥只存在于凭证私密半；额度与有效期随凭证 | 8 项测试 + 变异验证（`internal/credential/credential_test.go`） |
| A2 加入码自举 | `internal/cli`：`cmesh join --cred <串>` 一条命令完成凭证解码、证书签发、加入 | e2e：三进程拓扑一条命令入网，`cmesh status` 可见 |
| A2.7 客户端 `--cred`（**已落地**） | `internal/cli/join_cred.go`：解码凭证 → 本机签发自己的证书（委派私钥不出本机）→ 首次 announce 之前先 POST 准入 → 之后走正常 join 流程；无设备身份时自动创建 | 端到端测试：从未被签名过的设备用一串凭证入网、出现在名册、委派记录随名册持久化并在重载后仍在 |
| A2.5 节点侧准入（**服务端已落地**） | `internal/relay`：`POST /mesh/enroll`（仅在装了准入回调时挂载；共享出口限流预算同 register）→ `internal/cli` 的信任判定：验凭证签名/有效期/额度 → 验委派签名证书 → 记录委派 → 落名册并持久化 | 8 项测试（准入成功/篡改凭证/外部委派者/过期/额度用尽/跨账号/无账号公钥/非委派证书）；**客户端 `join --cred` 仍待做** |
| A2.6 快照带委派（**已落地**） | `internal/discovery`：`MemberSnapshot` 携带委派列表，客户端遇到 `SignerPub != nil` 的证书时按委派验签，而不是当伪造丢掉；`identity.MemberList` 新增委派段（Add/Remove/DelegationFor/DelegatedCount + 持久化） | 新设备出现在所有成员视图里（含未持凭证的旧成员）；无委派记录的委派证书仍被丢弃 |
| A3 端口自检（**已落地**） | `internal/cli/portcheck.go`：锚点启动时列出 TCP/UDP 端口占用状态，并给出可执行动作（开哪个端口、开哪个协议、回环绑定的后果） | 5 项测试；真实二进制输出已核验（含「同端口号 TCP+UDP 都要开」这条最容易漏的提示） |
| A4 凭证撤销传播 | 撤销后既有会话在一个推送往返内销毁（R-1.3） | e2e：撤销→旧会话断、新握手被拒，≤30 s |

**注意**：注册请求与配对 /ws 共用**按出口共享**的限流预算（120/min + 2/s）。
一键入网若让多台设备同时敲门，会复现 round 34 的 429 饱和。凭证改造时应把预算
**按设备凭证切分**（这正是审批延迟 sub-3.5s 的前置条件，见
[[container-mesh-status-snapshot]] 判据修订）。

## 2. (b) 可视化面板（锚点轻量版）

| 步 | 落点 | 验收 |
|---|---|---|
| B1 真实数据接口 | `internal/relay`：`GET /api/mesh` 返回 {account_id, version, generated_at, summary{members,online}, members[{device_id, online, endpoints, last_seen, is_anchor}]} | 单测：online 随成员启停自动变化；离线成员仍在列表 |
| B2 面板路由 | `GET /panel` 伺服内嵌 HTML；数据源只有 /api/mesh | 断网→顶栏告警不白屏；Network 面板除 /api/mesh 无出站请求 |
| B3 视觉落地 | 采用 `design-prototypes/aistudio-v1` 已验收的视觉与交互（保留成果，不重设计） | 网主看图确认；单文件 ≤100KB、零外部依赖、零 CDN |
| B4 授权边界 | 面板数据受 R-2.4 约束：非成员不可枚举网内设备 | 未授权请求 401；`/panel` 需带会话凭据或仅绑本机 |

B1/B2 是 Go 侧工作，与 (a) 共用锚点进程；B3 是前端单文件产出，可与 B1/B2 并行。
**注意**：原型里演示数据在断网时会回落到样例（「失败变演示成功」），生产实现
必须去掉该回落路径，真实模式只呈现 loading/stale/error。

## 3. (d) 分发

| 步 | 落点 | 验收 |
|---|---|---|
| D1 交叉编译矩阵 | `packaging/`：linux amd64/arm64、darwin amd64/arm64、windows amd64 | `packaging/build-demo.sh` 产出 5 个平台并校验 sha256 |
| D2 安装脚本 | `curl … \| sh` 形态，含架构探测、目录创建、systemd 单元 | 干净容器内一条命令装成；卸载有对应路径 |
| D3 固定下载地址 | GitHub Release 资产 + 稳定 URL（版本无关的 latest 别名） | README 里的地址与实际资产一致（脚本自动核对） |
| D3.1 latest 别名 | 发布环节把每个平台二进制**同时**以 `cmesh-latest-<os>-<arch>` 名义上传一次——GitHub 的 `releases/latest/download/<name>` 只对名为 `latest` 的资产生效 | `curl -fsSLO .../releases/latest/download/cmesh-latest-linux-amd64` 能取到文件 |
| D4 README 重写 | 面向中文用户：一条命令入网、面板地址、常见错误码 | 新用户按 README 走通一条命令入网 |
| D5 完整性校验命令（已落地） | `cmesh verify --file PATH --checksums SHA256SUMS`：纯本地校验，不下载不执行被校验物 | 实测：匹配 rc=0、被篡改 rc=1 且拒绝执行 |
| D6 发布合并检查单（**必读，方向已按 doubao 第 57 轮更正**） | main 与 develop 已分叉：**develop 领先 39 笔、main 领先 7 笔**，develop 历史线性（从未合入 main），main 仍停在 stage-1（`internal/relay/mesh.go`、`panel.html` 在 main 上都不存在）。`git diff main develop` = 89 files / **+21044 / −498**。**读法**：**+21,044 是 develop 独有的实现**（面板、凭证、mkrelease、R37 系列修复等）——合并时必须**原样保留**，它不是「删除」；**−498 才是 main 独有、develop 没有的内容**，那才是需要逐行看的一侧（构成：README.md 406 + docs/REQUIREMENTS.md 33 + 真实代码约 53 行 + 零星 docs） | 合并 develop→main 时：① +21,044 那侧**逐字保留**；② −498 那侧**逐行过**——README/需求文档以 develop 新版覆盖即可，**代码那约 50 行**要手工确认没有丢掉 main 独有的行为；③ 合并后 `git diff main develop --stat` 应只剩文档措辞差异；④ `git log --merges develop` 仍应为 **0**（保持线性发布历史），**不要整树取一边**（doubao-cloud-vm-01 第 56/57 轮提醒与更正） |

## 4. 排期与门禁

- 每日 17 点 KPI 汇报核销当天缺口（见 [[container-mesh-status-snapshot]]）。
- 每日门禁：`gofmt -l .` 无输出、`go vet ./...`、`go build ./...`、
  `go test ./... -race -count=1`、`bash tests/run-tests.sh --quick`、双架构交叉编译。
- 每批修复必须**红→绿**（先写会红的测试），测试进了仓库套件。
- 推 develop 走交互会话（值守会话不 commit/push）；main 只在成品版本、等网主放行。

## 5. 已知依赖与风险

- **共享限流预算**：一键入网 + 批量批准会放大 429；凭证切分预算前不做批量场景。
- **面板与 Wails 双轨**：轻量面板是容器内可用的最小形态；完整桌面应用按
  `architecture/APP-PACKAGING.md` 的三轨方案走，与面板共用 Go 侧接口。
- **中继路由推迟的连带影响**：S3 全受限场景的自动入网体验要靠人工转发兜底，
  v1.0 的 README 必须如实写明，不能含糊。
