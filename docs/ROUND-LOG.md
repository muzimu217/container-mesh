# Round 报告日志

> 首席协调者每轮迭代结束时的报告。格式见 [`TEAM.md`](TEAM.md) §3。
> **没有报告不算一轮结束。**

---

## Round 1 — PR-M1 身份层（2026-10-24）

**交付**：设备密钥对 / 账号设备证书 / 版本化成员表 + `init`、`account create|device-add|show` 四个 CLI 命令。合并 `aaf515e`。
**对应需求**：R-1.1、R-1.2、R-1.3、R-1.4、R-1.6（部分：元数据随证书）

| 角色 | 结论 | 关键发现/否决 |
|---|---|---|
| ②需求 | ✅ | 五条 R-1.x 全部有可判定验收；R-1.5（邀请授权）推迟到 M2 的 `join` 命令，已在需求表标注 |
| ③开发 | ✅ | `internal/identity` 零项目内依赖（地基约束成立）；TTL 默认值缺陷是开发专家在自检中推翻的 |
| ④测试 | ✅ | 红绿验证：破坏 `Verify` 签名检查 → 7 项断言红，恢复 → 绿。**额外**：CI 拦下 `TestSendWindowBoundsInFlight` 空转——破坏窗口门它依然绿，因为 pacing 才是约束（两个陷阱：WriteTo 延迟串行化发送方；keep-alive 预热 minRTT）。已修并红绿验证（`in flight reached 84`） |
| ⑤安全 | ✅ | 私钥 Argon2id(64MiB) 加密、文件 0600、测试断言原始文件无私钥明文；口令 ≥8 字符 |
| ⑥性能 | ⚪ | 身份层不在热路径，无 P 门槛涉及；Argon2id 64 MiB 的解锁耗时未测（记入遗留） |

**遗留**：
1. Argon2id 解锁耗时实测（预期 <1 s，超了要降参数）→ 转 M2 顺手测
2. R-1.5 邀请授权 → M2 的 `join` 实现
3. p2p2 死代码 2283 行 → 技术债窗口

**下一轮**：PR-M2 发现层。理由：M3/M4 都以「知道对端是谁、在哪」为前提；且它验证 R-2.4（发现层不向非成员暴露）这条安全需求。

---

## Round 2 — PR-M2 发现层·第一刀（2026-10-24）

**交付**：`internal/discovery` 状态机——announce 签名验证（成员资格/伪造签名/时钟偏差/端点形状四道检查）、Node 的注册/心跳超时/变更推送/在线快照、邀请编解码。约 380 生产 + 330 测试。
**对应需求**：R-2.4（非成员注入被拒）、R-2.2 的状态机半边（HTTP 断连上报未做）
**未完**：relay 挂载 `/mesh/*` 端点、客户端心跳循环、`join`/`invite`/`serve-discovery` CLI → 转 Round 3

| 角色 | 结论 | 关键发现/否决 |
|---|---|---|
| ②需求 | ✅ | R-2.4 验收可判定（外部者拿真实密钥+合法地址仍被拒）；R-1.5 顺延 |
| ③开发 | ✅ | discovery 只导入 identity + 标准库，不碰 mesh/e2e；邀请无秘密（只有账号公钥与发现地址） |
| ④测试 | ✅ | 双红绿：删成员检查 → `TestAnnounceRejectsNonMember` 红；把伪造签名改为接受 → `TestAnnounceRejectsForgedSignature` 红。**第一次破坏写反了**（`if true OR !Verify` 恒真 = 永远拒绝），测试仍绿——恰好证明红绿验证必须核对破坏方向本身 |
| ⑤安全 | ✅ | 时钟偏差 ±5 s 防重放；端点上限 16 防滥用；sweep 只过期「可达性」不动「成员资格」（睡眠两分钟的笔记本不该被重新审批） |
| ⑥性能 | ⚪ | 无热路径 |

**遗留**：发现层 HTTP 半边 + CLI；Round 1 的 Argon2id 解锁耗时仍未实测

## Round 3 — 中继 splice 零分配读（2026-10-25）

**交付**：`wsconn.RecvInto`（NextReader + 缓冲复用）+ 中继泵接入。`5c354dc` + 本轮。
**对应需求**：R-1.1 的中继路径吞吐（P-8 间接）

| 角色 | 结论 | 关键发现/否决 |
|---|---|---|
| ②需求 | ⚪ | 无新编号：这是 P-8（中继消息数）的底层改进 |
| ③开发 | ✅ | 调研确认 gorilla ReadMessage 每消息一分配（[pkg.go.dev](https://pkg.go.dev/github.com/gorilla/websocket)、[SO 65616177](https://stackoverflow.com/questions/65616177/how-do-writemessage-and-readmessage-work-in-gorilla-websocket-work)）；RecvInto 只给「收→发→丢」的调用方用，保留 Recv 给持有字节的调用方——别名风险写进了方法注释 |
| ④测试 | ✅ | 红绿验证：移除缓冲增长逻辑 → 测试**挂死超时**（30.5 s FAIL，读循环在满缓冲上空转）——挂死本身就是证据；恢复 0.36 s 绿。4 项新单测（往返/跨调用复用/文本帧跳过/错误传播） |
| ⑤安全 | ✅ | RecvInto 语义与 Recv 完全一致（同锁、同 deadline、同 markGone）；文本帧照旧丢弃 |
| ⑥性能 | ✅ | **A/B 同刻对照**（同一台机器、同一 benchmark 会话）：<br>• Recv 路径：158,497 B/op、32 allocs/op<br>• RecvInto 路径：**55 B/op、1 alloc/op**<br>分配降 **2,872 倍**；服务端读耗时 22.3 µs → 4.5 µs（4.9×） |

**注**：`BenchmarkRecvIntoServerSide` 测的是服务端纯读（发送端在客户端），与 `BenchmarkSendRecv32K` 的往返不直接可比——**可比的是 allocs/op**，两者同消息尺寸同时刻。吞吐数字记录但不作为回退门槛（回环噪声大，见条约 T-2.6）。

**遗留**：直连双服务器实测（等用户提供两台机器后做真 WAN A/B）；p2p2 删除仍在技术债窗口

**下一轮**：PR-M2 第二刀（relay `/mesh/*` 端点 + 客户端心跳 + join/invite CLI），回到组网主线

## Round 4 — 工作团实战首秀：外部评审 + 5 项安全修复（2026-10-31）

**交付**：工作团配置（WORKTEAM.md + 6 份提示词模板）+ 两次真实委派（codearts 文档核对、claude 安全评审）+ 5 项安全修复与 7 项回归测试。
**对应需求**：R-2.4 的加固；文档一致性

### 委派实测

| 委派 | 代理 | 产出质量 | 采纳 |
|---|---|---|---|
| 文档机械核对 | codearts (deepseek-flash) | **5 处真实不一致**：帧数 8→7、MESH-DESIGN §0 行数未随正文更新、评审判定表 13 项但抬头写 14、relay.go:318 引用措辞不精确 | **全部修复** |
| discovery 安全评审 | claude | **9 项发现（高1/中3/低5）**，全部附 file:line | 高+中+可修低共 **5 项修复**，4 项为设计级（记入 MESH-DESIGN 遗留） |

### claude 评审的核实与修复

| 发现 | 核实 | 处置 |
|---|---|---|
| `Register` 无内建认证（高） | ✅ 成立 | 加结构后备：`Contains` 检查在 Register 内部，调用方忘 Validate 也挡住；红绿验证 |
| 端点控制字符放行（中） | ✅ 实测：`evil\nFAKE:80` 被接受 | 加控制字符拒绝 + `@`/空格 host 拒绝 |
| `memberKey` 长度不校验可致 panic（中） | ✅ 实测：`ed25519.Verify` 对 5 字节公钥直接 panic | 显式长度检查 |
| `Subscribe` 覆盖旧 channel 不关闭 | ✅ 成立 | 二次 Subscribe 先 close 再替换 |
| invite `disc` 无上限 | ✅ 成立 | 上限对齐 `MaxEndpoints` |
| Register 保存调用方 slice 别名 | ✅ 成立 | 改为 copy |
| 成员可签名任意地址（SSRF） | 设计级 | 记入 MESH-DESIGN 遗留（阶段 3 解决） |
| 错误回显对端地址 | 待传输层实现后 | 记入 MESH-DESIGN 遗留 |
| 成员枚举 oracle | 低危害 | 记录不修 |

### 教训

外部代理的结论**同样需要核实**：codearts 说 `relay.go:318` 处没有 `roles["A"]/["B"]` 字面量——事实，但该行**确实是**我们要改的两人配对校验，只是措辞是 `role != "A" && role != "B"` 而非 roles map 访问。核实不是找茬，是分清「措辞不准」和「结论错误」。

**下一轮**：PR-M2 第二刀（relay `/mesh/*` + 客户端心跳 + CLI）

## Round 5 — 需求探索：双模型审计 A/B + R3 修订 + 模型漂移处置（2026-11-03）

**交付**：REQUIREMENTS.md R2→R3（探索采纳项 + 状态列回写）；ROSTER.md 模型漂移处置。本轮无代码改动——按变更规则「先改需求文档再改代码」，赶在 PR-M2 第二刀动工前把需求面理清。

### 需求探索：双模型独立审计

同一份 9 问提示词（5 审计 + 4 探索），材料内嵌进提示词（不依赖代理自己读文件），两个模型独立作答：

| 模型 | 质量 | 关键差异 |
|---|---|---|
| doubao-1.5-pro（claude CLI 默认，漂移后兜底） | 机械核对可用 | 抓到 probe/wsconn 无编号覆盖；但「幽灵需求」有一处误标（诚实标 ❌ 的未实现项不是幽灵需求） |
| GLM-5.3（`--model` 显式指定） | **显著更强** | 抓到三处数字矛盾（R-4.1 10ms vs P-1/§5.1 5ms）、P0 验收依赖 P1 的结构问题、§1.3 交叉引用错误（R-5.2→应为 R-5.4）、R-3.5「不知道」是认知命题不可测 |

**A/B 结论**：推理岗必须显式指定强模型；flash 档 + 材料内嵌足以承担机械核对。

### 采纳（已写入 R3）

- **[doubao]** 新增 N-7（无 MagicDNS）/ N-8（跨身份不做）/ N-9（全网不可达无解）；C-5（WebSocket 出站穿代理，U3 准入条件）；O-5~O-8（邀请规范/自举零配置/端口冲突/`cmesh doctor`）
- **[GLM]** §1.3 引用 R-5.2→R-5.4；R-4.1 数字统一到 P-1（<5 ms p95）；**R-2.3 P1→P0**（阶段 2 验收的可观测面不能是 P1）；R-3.5 改写「不知内容、知端点」；R-1.5 改可判定形式；N-3/N-5 论据补全；N-10（无 root 不能听 <1024）；R-1.4 补 K8s Secret 挂载注入；O-5 补「可撤销」；§5.1 重构（去重行、补 P-2/P-6/P-7 数字、p95 口径、撤销时限落地为 ≤30 s）

### 拒绝（附依据）

- 「p2p2 需要 R 编号」——p2p2 是已排期删除的死代码（技术债窗口），不给编号
- 「R-2.5(P1) 是 R-1.3(P0) 的前提」——撤销传播依赖的是发现层 R-2.1/R-2.2（均 P0）；R-2.5 讲的是无中心韧性，不是传播路径
- 「安装/升级需编号」——C-4 已覆盖协议面，版本策略属发布实践

### 模型漂移事件（当天发现、当天处置）

claude CLI 默认模型在 10-31 ~ 11-03 之间被 magpie 路由**静默改为 doubao-1.5-pro**（settings.json `/model` + ANTHROPIC_DEFAULT_OPUS/SONNET/HAIKU 全部映射豆包）；显式指定 `github-copilot/claude-opus-5` 返回 400 无权限。靠输出 banner 的 `unrecognized_model` 才抓到。
**处置**：所有委派显式 `--model` 并核对输出自报；安全/性能岗改派 `auto/glm-5.3`（本轮 A/B 即转正依据）；ROSTER 抽查频率从每 3 轮提到每轮。

### doc-check 复核（codearts，提交前）

抓到 4 处本轮问题并当场修复：R3 头漏数 N-10（实增 4 条）；§5.1 撤销措辞与 MESH-DESIGN「不是等心跳」打架（改为「推送机制 + 30s 宽限上限」）；docs/README.md 状态行滞留「尚未实现」；§2.1 U2 行未标注开放决策。编号序列核对：R/N/P/C/O 全序列无重复无跳号，断链 0、孤儿编号 0。
**遗留（历史遗留，非本轮引入，记入技术债窗口）**：DESIGN.md 与 MESH-DESIGN.md 的行数预估互相矛盾（500vs650、700vs850、2250vs2550、"1,900" 引用失效）、DESIGN.md 内部 R-1.1–R-1.5 与 R-1.1–1.6 两处范围不一致。

**开放决策（待用户）**：U2 小团队方向——共享团队身份（推荐，不扩范围）vs ACL 立项 vs 明确不考虑
**下一轮**：PR-M2 第二刀（relay `/mesh/*` 端点 + 客户端心跳 + join/invite CLI）

## Round 6 — PR-M2 第二刀：发现层传输 + CLI 组网闭环（2026-11-03）

**交付**：发现层 HTTP/WS 传输落地，组网从「状态机」变成「可用 CLI 流程」。共 ~1,190 行生产代码 + ~890 行测试。
**对应需求**：R-2.1/R-2.2/R-2.4（发现）、R-1.5（显式授权）、R-1.6 部分（设备元数据）、O-5/O-6/O-7 部分（邀请/自举）、C-3（零入站端口可见性）

### 新增

| 块 | 内容 |
|---|---|
| `relay/mesh.go` | `/mesh/register`（签名心跳，限流 + 64KiB 上限 + 严格 JSON）、`/mesh/members`（成员认证 WS，全量快照推送）。**双门纵深**：成员查找与签名验证是两道独立检查 |
| `discovery/snapshot.go` | MemberSnapshot/MemberView/BuildSnapshot——成员表（谁属于）× 节点存活视图（谁在线）的合成 |
| `discovery/client.go` | Client：30s 心跳、更新流（快照全量 + 信号合并）、**多地址轮转容灾**（死节点只花一个超时）、**快照验证**（每个证书对账号公钥验证，恶意/被攻破发现节点无法伪造成员——只能拒绝服务） |
| `identity/account.go` | `IssueCertForPub`——远程公钥签发（审批者从未见过对方私钥，设备 ID 由公钥推导） |
| `cli/discovery.go` | `invite`/`join`/`serve-discovery` + 审批 watcher（certs 目录自动收编 + members.json 外部编辑热加载，错误账号文件拒收） |
| `cli/mesh.go` | `account authorize`（远程签发）、`show-pub`（导出公钥） |

### 三节点互见验收（R-2.2 / §5.1）

集成测试：3 节点各自心跳，每个节点在 8s 内看到全部 3 个成员在线（要求 ≤10s）。另钉死反例：未批准设备**双盲**——它读不到名册（流 401），别人也看不到它（不出现在视图）。

### 红绿验证（T-2.1）

| 破坏点 | 红测试 |
|---|---|
| register 签名/时间戳验证（Validate） | BadSignature、StaleTimestamp 红 |
| register 成员查找（meshMemberKey） | NonMember 红（破坏 Validate 时它仍挡住——纵深生效） |
| members 流签名验证 | **新增** corrupted-signature case 后红（原测试集漏了这个 case，红检暴露的） |

### CLI 全流程测试（真实端口，同进程两台「机器」）

init → account create → device-add → serve-discovery（自举 members.json）→ invite → 第二台 init → show-pub → join → account authorize（远程签发）→ 证书丢进 certs/ → watcher 收编推送 → joiner 获批返回 → members.json 持久化 2 成员。

### GLM-5.3 安全评审（材料内嵌委派，11 项发现：高 1 / 中 4 / 低 6）

这是新安全岗（`claude --model auto/glm-5.3`）的转正评审。逐条核实：**10 项属实并当场修复，1 项（F7）为阶段 1 既有先例的一致性偏差，记为已声明偏差**。

| # | 级 | 结论 | 处置 |
|---|---|---|---|
| F1 | 高 | 快照 Version 从不比对——注释声称的保护不存在；恶意节点重放撤销前旧快照可**复活已撤销成员** | ✅ 客户端 maxVersion 单调性（降版拒绝、同版应用）+ 重载版本 bump + 回归测试（重放被拒） |
| F2 | 中 | certs 目录收编证书零验签零验期 | ✅ serve-discovery 新增 `--account-pub`（提供则收编前验签）；未提供则启动警告 + 过期必查 |
| F3 | 中 | 一坏全拒 + 服务端不滤过期证书 → **365 天 TTL 到期即全网视图熔断** | ✅ 双侧修：客户端坏成员剔除不熔断；BuildSnapshot 滤过期；测试钉死「好成员在坏成员旁存活」 |
| F4 | 中 | members 流缺公钥长度检查（announce 路径有且标注 load-bearing）→ 可构造 panic 杀死进程 | ✅ 根修在 `VerifyDevice` 原语层（长度不符返回 false）+ 毒化记录测试（修复前必 panic） |
| F8 | 中 | 恶意节点 4xx 响应体未消毒进客户端日志（换行/ANSI 伪造日志行） | ✅ printableBody 控制字符转空格 + 测试 |
| F5 | 低 | 客户端不复验快照中的 Endpoints | ✅ 复验形状（validEndpoint），无效剔除 |
| F6 | 低 | 拒绝日志在 Valid() 前记录任意 DeviceID；reason 携带端点串/到期时间（T-4.2 冲突） | ✅ reasonCode 稳定令牌 + safeID 消毒 |
| F9 | 低 | join 写盘先于校验 | ✅ 全部校验通过后才落盘 |
| F10 | 低 | `--invite-ttl` 帮助文本谎称「join 时强制」实无检查 | ✅ 文本如实 + join 拒绝未来时间戳（完整 TTL 强制归 O-5 跟进） |
| F11 | 低 | TLS 半配置静默降级明文 | ✅ 报 usage 错误退出 + 测试 |
| F7 | 低 | 限流日志记客户端 IP | ⚠️ 接受：与阶段 1 relay.go 限流日志一致（既有先例），记为已声明偏差 |

**评审质量注记**：F1 抓的是「注释声称的防线代码里没有」——正是 A/B 审计不可能发现的实现级漏洞，证明重模型 + 源码材料内嵌的委派方式有效。

### 教训

1. **红检暴露测试缺口**：members 签名门破坏后测试仍绿——没有「成员持坏签名」的 case。红绿验证不只是确认「门坏了会红」，也是确认「每个门都有自己的红测试」。
2. **flag.Parse 在第一个位置参数处停止**：`cmesh join <invite> --state X` 会把 --state 吞进位置参数。invite 必须放最后，usage 提示写明。
3. 快照推的是**全量视图**而非增量：几十台规模下，增量协议的排序复杂度不值得，错过的推送等下一个全量即可。
4. 「一坏全拒」看着更安全，实际把单点失败放大成全网故障——剔除 + 告警优于整体拒绝。

**下一轮**：PR-M3（直连：p2p/p2pquic 接入组网候选地址，`dial`/`mesh ping`；QUIC 显式超时按评审备忘）

## Round 7 — PR-M3：组网直连层 + mTLS 设备证书 + 部署指南（2026-11-03）

**交付**：任意两成员直连（`mesh ping` / `dial`），端点从成员视图解析，双向以设备证书认证。P-1 实测达标：**同机 p95 0.203ms**（门槛 5ms）。
**对应需求**：R-3.1 部分（组网直连层）、R-4.1 部分（直连 RTT）、P-1、P-5（QUIC 显式 10s/3s）、O-6（首台自举）、deploy 指南

### 新增

| 块 | 内容 |
|---|---|
| `internal/mesh/direct.go` | Listener + DialDirect：端点按序尝试；**设备 Ed25519 身份密钥即 TLS 证书**（`identity.TLSCertificate`），拨号方把对端证书公钥**钉死**到成员视图公钥，监听方 mTLS 验证客户端证书——没有 CA、没有主机名，成员视图就是信任锚；撤销组合生效（除名→证书不再钉中→立即拒连，有测试钉死） |
| QUIC 超时（评审备忘落实） | `MaxIdleTimeout 10s + KeepAlivePeriod 3s` 显式配置——拔网线/断电/容器 netns 销毁不发任何包，只靠 idle timeout 感知 |
| `identity.TLSCertificate` | 自签证书：公钥=身份密钥、CN=设备 ID、IsCA 自根 |
| CLI | `cmesh mesh ping`（RTT 统计 p50/p95）、`cmesh dial`（交互直连）、`join --listen`（绑定→公告一致：端口 0 自动解析后公告实际端口） |
| `deploy/MESH-NET.md` | 组网部署指南：公网/无公网混合拓扑、凭证打包三原则（容器只放 device.json+cert+discovery.json，account.json 永不出信任锚）、Docker/systemd 示例、六项实验清单、故障排查 |

### 平台行为实测记录（go1.27.2/darwin）

1. **`VerifyConnection` 不替代默认验证**：自签 Ed25519 证书在 VerifyConnection 触发前就被 macOS 系统验证器拒绝（错误串 "certificate is using a broken key size" 不存在于 Go 源码——来自系统验证器）。必须跳过默认验证，钉死验证才运行。实测两种配置的完整对照后定案；**验证没有被禁用，是被更严格的成员视图钉死替代**（错误密钥→握手失败，有 impostor 测试钉死）。Mimosa 扫描器对字面量的拦截以此说明回应。
2. x509.Verify 对自签证书**必须**显式 `Roots` 池（自身为根），系统池路径必挂。
3. 设备证书模板需要 `IsCA + BasicConstraintsValid`（自根的最小 CA 形态）。

### 测试

- mesh 包 6 项：ping 往返（p95 烟雾线 50ms，实测打印）、duplex 回显、**impostor 拒绝**（错误证书公钥→握手失败）、非成员拒绝、**撤销即拒连**、mTLS 平台行为回归
- CLI 全流程测试加第三腿：owner `join --listen` 自加入 → joiner `mesh ping` 全栈闭环（发现→视图解析→直连→RTT）
- 全量：`go test ./... -race` **15 包全绿**（新增 mesh 包），e2e 35/35，vet 干净

### 教训

1. **生成过程自检**：本轮两次把损坏内容写进源文件（乱码混入），靠「每次写入后立即 build」当场抓回。写入后的编译检查不是仪式，是安全网。
2. 「注释说 X、代码做 Y」的坑上一轮 F1 刚修过，这轮 VerifyConnection 的官方语义与实测行为又不一致——**平台行为要实测记录，不能只读文档**。

**下一轮**：PR-M4（e2e SIGMA 层 + 中转路由 + 配额——风险最高的 PR，单独提交便于 bisect）；真机实验等用户服务器就绪后按 deploy/MESH-NET.md 清单执行

## Round 8 — 外部测试回报处置：1 高危功能阻断 + P-5 失败根修（2026-11-07）

**输入**：issue #8 两份独立外部测试报告（单沙箱三进程版；netns+iptables 真实 NAT 拓扑版）。质量极高：jsDelivr sha256 逐文件锁定 commit、真实内核转发路径搭 NAT、每条缺陷带代码定位+复现命令、未验证项如实单列。**全部接受**，处置回复已发 issue。

### 测试结论（develop @ f0124e9）

8 项：6 通过 / **#7 kill -9 收敛失败（69–83s vs ≤10s）** / #5 一半被 D-1 阻断。另 6 个非已知边界缺陷（D-1~D-6）+ 4 个文档/易用性问题。

### 根修（本 Round 交付）

| 发现 | 根因 | 修复 |
|---|---|---|
| **#7 P-5 失败**（两报告一致） | 成员视图 online 只看心跳（90s+15s）；R-2.2 说的「传输层断连检测」**从未实现**——发现节点对成员只有 POST，没有可断的连接。issue 文案把 QUIC 链路 idle 和成员存活混为一谈（认账） | **快路径**：节点按设备计数 members 流（AttachStream/DetachStream），流断开→立即清在线+推送。**排水读取器**：server 原本不读该流，客户端 FIN 要等 ping 周期才发现（红检抓出：20s→0.01s）；liveness 收紧 3s ping/10s idle（对齐备忘数字，硬杀场景 ≤10s）。心跳超时降级为「连接活着但进程僵死」的兜底 |
| **D-1 高危阻断** | `--listen 0.0.0.0:7000` 把通配地址当端点广播→对端拨自己的回环；`--endpoint` 被 BoundAddrs 覆盖 | BoundAddrs 滤未指定地址；`--endpoint` 显式优先（NAT 映射场景的正解）；通配-only 绑定大声警告；DialDirect 拒绝通配端点（纵深） |
| **D-3 watcher 死循环** | 外部编辑检测用 mtime，自己 SaveMembers 刷 mtime→自触发（原始日志实锤 201 次，v3→v208） | 自身保存后重读 mtime 记账；坏文件/拒收也记账防重试风暴 |
| 3.2 日志热循环（164MB/10min） | D-3 版本灌水 + join 对同版本快照反复渲染 | D-3 修复 + join 打印加「版本+在线集合」签名守卫（在线翻转仍可见） |
| D-2 hex/base64 陷阱 | account show 输出 hex、--account-pub 只收 base64 | 双编码接受 + account show 加 pubkey.b64 行 |
| D-4 提示不可达 | 「在线但无端点」等满 20s 才报通用超时 | 该状态是稳态，立即给可操作提示 |
| D-5 口令不一致 | mesh ping/dial 没走 readPassphrase | 统一走（CMESH_PASSPHRASE 恢复可用） |
| D-6 发现节点恒 offline | 节点不 announce | `--announce` 参数 + 具体 --addr 自动公告 + 通配时如实说明 |
| 报告一 #8 文案 | 撤销后报「网络不可达」误导排障 | openMeshSession 失败时补 announce 探测，401 not-member→明确报「已被移出网络」 |
| 3.1 构建非静态 | issue 命令缺 CGO_ENABLED=0 | 指南修正（Makefile 本来就对） |
| 3.3 间歇 dial 超时 | 未复现 | 主嫌疑 D-3 风暴；修复后请测试者复测（已在 issue 回复注明） |

### 红绿验证

- **TestStreamCloseExpiresMemberFast**：先红后修——第一版只挂 Attach/Detach 时测试红了（推送要等 ping 周期），暴露「server 不读流」的真问题，排水读取器是红检逼出来的
- TestStreamOverlapKeepsOnline（重连重叠不误杀）、TestDialRejectsWildcardEndpoint、TestBoundAddrsFiltersWildcard、**TestWatchApprovalsNoSelfTriggerLoop**（空闲 5.5s 版本不变，D-3 修复前必红）

### 全量验证

`go test ./... -race` 15 包全绿，e2e 35/35，vet 干净。

### 教训

1. **「服务端只写不读」的流必须排水**：不读就发现不了对端关闭——这次是红检把 20s→0.01s 的差距暴露出来的，不是推演。
2. issue 邀请文案里每个技术断言都会被当验收标准执行——「QUIC 10s idle」被写成 #7 的保障，测试者按它判了失败。**文案=契约**。
3. 外部测试者搭 netns+NAT 真实拓扑的严谨度超过我们自己的验收测试——真机实验清单应该吸收他们的复现脚本（§7 最短路径已可复用）。

**下一轮**：等 #8 复测回报（重点：#7 收敛、D-1 修复后公网→NAT 方向、3.3 是否随 D-3 消失）→ PR-M4（e2e + 中转路由）

## Round 9 — 复测处置：两项指控反驳 + 慢消费者踢除加固（2026-11-07）

**输入**：复测报告（develop @ `ba1619f`）——声称 #7 kill -9 回归为流堵死冻结、N-1 新发现端点污染、D-2 NOTE 误报；随包日志 tar.gz。

### 核实结果（第一方复现 + 随包日志交叉）

| 指控 | 结论 |
|---|---|
| N-1 拨发现地址 20s 假拨号 | **反驳**：同拓扑复现 131ms 即时提示（`resolveTarget` 对空端点稳态立即短路，无回退路径） |
| #7 kill -9 冻结 110s | **反驳**：同拓扑复现 **0.56s** 翻 offline，kill 后新成员变更正常推送；随包日志（netns 复测）独立佐证 **1.1s** |
| D-2 NOTE 误报 | **部分成立**：NOTE 是诚实告警（flag 到达时为空），但 `account show` 公钥打 stderr 是真缺陷 → 修为 stdout 输出（`pubkey.b64`/`pubkey.hex`），与 show-pub 约定一致；我方复现脚本同样踩中（`2>/dev/null` 拿空） |

**关键疑点**：随包日志是 **netns 拓扑**（`ws://8080`、pub2/memB 角色名、t7b kill-9=1.1s），与报告文本所述单机 A/B/C `:18080` 拓扑不符——现场日志（`/tmp/cmtest2`）未随包，已请报告作者补交四件证据。`logs/accountpub.txt` 的「公钥」解码开头是 ASCII `#tes`（非均匀随机形态）+ `PUBB64=` 变量赋值行，指向 harness 取值链路拿到错误对象。

### 采纳的加固（报告建议正确的部分）

`streamMeshMembers` 每次 push 带 **5s 写超时**，超时踢除慢消费者（无界 Send 会灌满内核缓冲、卡死 keepalive、冻结视图——失败形态真实存在，即使本轮未能复现）。踢除安全：客户端重连即拿新快照。

**红检插曲**：踢除测试第一版 20s 红——不是机制没生效（日志实锤 i/o timeout + conn 关闭），而是**断言选错**：楔死的客户端自己永不读、无 liveness，`Closed()` 不会自触发；改为轮询「服务端关闭在客户端读侧浮出」，2s 绿。

### 交付

`account show` stdout 修正、wsconn `SetWriteDeadline`（增量方法）、push 写超时踢除、`TestSlowConsumerKicked`、指南取值示例。issue #8 三条评论（代发复测报告 / 本处置）。全量 `-race` 15 包 + e2e 35/35 绿。

### 教训

1. **随包证据要先自洽再定结论**：tar.gz 里的日志与 md 报告描述的是两个不同环境的运行——若只读 md 就动手改代码，会把两个真 bug（Round 8 已修）再「修」一遍。
2. 断言可观测量的选择和机制本身一样重要：楔死客户端的 `Closed()` 在结构上不可能触发，轮询读侧才是「踢除」的正确观测量。
3. 脚本取值面（stdout）与人类叙述面（stderr）的约定必须全 CLI 一致——一处不一致（account show）就让外部 harness 与我自己的复现脚本同时翻车。

**下一轮**：等报告一作者补证据 / netns 测试者确认 → develop→main 合并 PR → PR-M4

## Round 10 — 三方复测收敛：D-7 根修 + 端点语义纠正（公告态 ≠ 存活态）（2026-11-07）

**输入**：三份复测（3e1397d 沙盒版全过无阻塞；ba1619f 版定位端点振荡根因；netns 版确认两项反驳 + 抓出 **D-7 新阻断**）。

### 本轮核心：端点语义纠正

三方证据合流指向同一个设计错误——**把「公告态」（endpoints）当成了「存活态」（liveness）**，而我们自己的 Sweep 注释早就写着 "membership and liveness are different things"。两个表现：

| 症状 | 机制 | 修复 |
|---|---|---|
| **D-7（高，阻断）**：跑一次 `mesh ping` 后本机 30s 不可被直连 | 一次性客户端 `Client.Run` 立即以空端点 announce，覆盖 join 进程公告的端点 | 双层：① `NoAnnounce` 视图模式——mesh ping/dial 只取视图不当第二公告者；② 服务端 `Register` 空端点=纯在场，**不覆盖**已公告端点（防未来同类 bug） |
| **端点振荡（~30s 有/无，报告二定位）** | WS 空闲超时重连存在空窗，`DetachStream` 把端点跟 lastSeen 一起删，恢复要等下一次心跳 | `DetachStream` 只删存活不删端点；`BuildSnapshot` 端点**离线也可见**（流断了不等于 UDP 监听没了——可拨穿）；Sweep 仍在 90s 地平线清端点 |

同源修复：D-6r（发现节点公告的地址挂上真正的 QUIC 监听，UDP/TCP 同端口号共存；绑不上就丢弃自公告）、D-1b（通配-only 打印「bound, not announced」而非空串）。

### 测试

- `TestEmptyAnnounceKeepsEndpoints`（D-7 服务端护栏：空 announce 刷新存活但不覆盖端点）
- `TestDetachStreamKeepsEndpoints`（detach 后端点保留、可拨穿）
- `TestStreamCloseExpiresMemberFast` 断言翻转：offline 后端点**保留**（原断言端点清空——语义修正的体现）
- CLI 全流程加腿 4：owner ping 提示后 joiner 再 ping owner **必须成功**（D-7 回归）
- 全量 `-race` 15 包 + e2e 35/35 绿

### O-1（Send-Q MB 级积压）：挂 PR-M4

慢消费者踢除已生效（本轮未堵死），但全量快照推送粒度对慢消费者仍会积到 MB 级。根治是增量推送，属于 M4 路由层工作；本轮不改阈值（5s 写超时是「防失控」线，不是「防积压」线——防积压需要协议层改动）。

### 教训

1. **「谁在公告」要唯一**：一台机器两个 announce 来源（join 进程 + 一次性命令）天然竞态。一次性命令应该是纯读者。这个错误是我写 `openMeshSession` 时引入的——三个测试者从三个角度（振荡、假拨号、不可直连）合力才定位到。
2. 状态分类学：membership（谁属于）/ liveness（谁可达）/ announcement（谁说自己在哪）是三种生命周期不同的状态——前两条我们分清了，第三条这次才分清。

**下一轮**：等三方在修复 commit 上确认 D-7/振荡/3.3 闭环 → develop→main 合并 PR → PR-M4（e2e + 中转路由 + 增量推送一并考虑 O-1）

## Round 11 — 基础通信补齐：按名消息 + 按名大文件 + 远端诊断动词（2026-11-09）

**输入**：用户指令「最基础的传输通信和命令行接入调用都要实现」+ Hub 值守轮次（三方 2891872 复测：D-7/振荡/D-6r 全部确认；oh-no 报告 #7 积压场景失效）。

### 交付（全部实测，三进程真机演示）

| 能力 | 命令 | 实测 |
|---|---|---|
| **按名消息**（R-4.1） | `cmesh msg <deviceID> --text "…"` | 实时投递 ✓，多行/中文 ✓，双向 ✓（无 listen 侧也能主动发） |
| **按名大文件**（R-4.2） | `cmesh send-to <deviceID> --file PATH` | **100MB @ 72.3 MB/s**，SHA-256 双端一致 ✓；头部承诺 hash→流式传输→接收端复验→verdict |
| **远端诊断动词** | `cmesh run <deviceID> @info/@disk/@mem/@cat/@ls` | 全部 ✓（Go 原生实现，无 os/exec——任意外名命令执行被 Mimosa 规则拦 5 次，含其白名单建议版；改为内置动词，任意外命令作为独立特性待规则调整） |
| **#7 积压场景 slow-kick** | 写入成功 >750ms 即踢除 | `TestThrottledConsumerKicked`：限速读取者 + 6 万风暴，修复前永久排队 → 修复后 1.31s 踢除 |
| **批准收敛加速** | 401 not-member announce 改 3s 快轮询 | 批准→端点可见不再等满 30s 心跳 |
| **端点可见性** | join 视图打印含端点列 | 排障不再盲 |

### 过程抓出的真缺陷（均为演示/红检暴露，非推测）

1. **serveStream dispatch 缺 KindRun/KindExec case**——白名单放行、dispatch 落 default，拨号方读 EOF。教训：**新增枚举值时 grep 所有 switch**。
2. **监听端 bufio 挂 LimitReader(4097)**——会话体被截断，8MB 文件 4KB 即 EOF、发送端永久阻塞。修：bufio 裸挂 + readLine 自身 cap 限制 hello；发送端文件体分块写 + 60s 滚动写超时。
3. **msg 半关闭竞态**——发送端写完立刻关连接会丢弃尾部帧；修复：接收方处理完回 `done` ack，发送方等到 ack 再拆连接（连接关闭竞态导致的静默丢消息，测试抓出）。
4. 演示脚本两次设备 ID 映射搞反——「错误提示恰好正确」的案例：发给无 inbox 节点被正确拒收、发给无监听节点收到可操作提示。

### 已知边界（如实）

- 任意外名命令执行（ssh 形态 exec）暂缺：Mimosa 扫描器拦 5 次（含白名单版），待规则调整后实现（exec.go 头部已留设计文档）
- 消息/文件暂无断点续传（阶段 1 两人模式有）；增量推送（O-1）挂 PR-M4
- 消息发往未配置 OnMessage 的监听方会静默丢弃（应回错误帧，挂 M5）

**下一轮**：三方将 msg/send-to/run 纳入回归 → develop→main 合并 PR（待放行）→ PR-M4（e2e + 中转路由 + 增量推送）
