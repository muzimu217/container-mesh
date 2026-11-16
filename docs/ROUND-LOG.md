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

## Round 12 — M4 首批：恢复可见性 / 传输诊断 / 统一凭证 / 驾驶舱面板（2026-11-13）

> 本条**合并记录** Round 12–56 的关键结论；逐轮明细见 issue #8 与 hub 归档。

### 1. 恢复可见性（C-4 / R39-1 / R40-1）

| 缺陷 | 根因 | 修法 | 实测 |
|---|---|---|---|
| 审批后设备迟迟不可拨（26s） | 公告快探（3s）知道批准了，却**不唤醒成员流**；流的重连梯子已爬到 30s 档 | 公告在「未入册」状态下首次成功即踢醒成员流 | 长待批臂 **3/3 ≤5s**（985/3926/859ms），红基线 26.1–26.7s |
| 成员回归后全网仍显示离线（最长 147.7s） | Register 只在**端点变化**时推送；成员带着同样的端点回来 → 无推送 | Register 在**离线→在线转场**时推送 | oh-no 恢复跟随 **0.25s**、doubao 上线 ≈1.1s |
| 恢复后自己看不见自己 | 流重连后拿到新快照，但没有公告 | 重连后首个通过验证的快照触发一次带外公告 | 与上一条合并，收敛到秒级 |

**判据修订（重要）**：审批→可拨统一按 **≤5s** 判定（长待批与短待批同口径）。原「短待批 ≤3.5s」作废——3s 公告节拍是共享出口预算下的安全值（实测 20 台同源 → 93 次 429），压到 2s 会回到饱和形态；sub-3.5s 需要**按设备凭证切分预算**（M4 后续项）。

### 2. 传输诊断（F42）

接收端存不下时（空间不足 / 只读卷）此前**静默丢会话**：发送方等 accept/reject 最多 30s、等结果最多 10 分钟，且永远拿不到原因。修法分两相：accept **之前**先打开目的地文件（失败即 REJECT 带根因）；传输中失败发 RESULT 带根因并**drain 掉剩余分片**（否则对端只看到传输层错误）。一次性 recv 的本地失败改为经同一回调上报失败裁决——否则它会挂到会话超时。

### 3. 统一凭证与一键入网（R-6.7，M4-a）

账号持有方签发**一行**凭证（带有效期与设备额度），成员机器 `cmesh join --cred <串>` **一条命令**入网：自动建身份 → 本机用委派密钥签证书 → 先准入再公告。**账号私钥不随凭证出门**：凭证带的是**委派签发密钥**，委派签出的证书用账号公钥验签**必然失败**（账号没签过它），只能凭名册/快照里的委派记录验证。

- 额度**从名册计数**：撤销设备归还额度、节点重启自动恢复。
- 快照必须携带委派记录，否则委派入网的设备会被当成伪造成员——**在网里却对所有人不可见**（本批最隐蔽的一个坑）。
- 已进仓库 e2e 门禁（第 11 节：签发凭证 → 全新 state 无身份 → 一条命令入网 → 名册含委派记录与委派签名成员）。

### 4. 面板 = 已验收的驾驶舱原型

原型（单文件、零外链）**照抄**进产品：3D 等距工作台、总览 HUD、成员名册栏、四维状态核验、诊断抽屉原样保留；只改三处——删掉演示场景与场景切换器（生产界面不提供假状态）、数据源改为真实 `GET /api/mesh`（5s 轮询 + 令牌头）、加令牌门。**Go 端按原型的字段名扩齐接口**，原型的渲染代码一行未改。轻量单文件版保留在 `/panel-lite`。

### 5. 429 同址互扰：基线更正（agent 自我更正，重要）

| 项 | 早先记的基线 | 复核结论 |
|---|---|---|
| N=8@300s 的 429 次数 | 86 次 | **复现不到**（实测 8 次）。差异来自公告节拍：本版 3s+抖动(≤600ms)，8 台聚合 ≈2.4 req/s ≈ 补充率 2/s，只在窗口末段临界打空；打空 120 令牌需聚合 ≥2.7 req/s（例如固定 3s 无抖动）或更长窗口 |
| 审批→可拨最坏延迟 | 33.7s | **口径存疑**：用「同 egress 的参照成员 ping 每一台」测量时，**探针自己也在抢同一个令牌桶**，等于装置抢了客户端的配额（N=16 时量到 122s）。改用客户端日志 `approved: this device is now a member` 后，N=2/8/16 分别为 1.72s / 2.10–2.39s / 1.79–3.75s，**全部 ≤5s** |
| 「429 全落令牌桶、并发上限 0 次」 | 假设 | **独立复现成立**：N=16 同源 → 156 次 rate、0 次 concurrency；64 路即时并发 → 仍 0 |
| 并发上限分支可达 | 未知 | **可达且文案正确**：24 路慢体 register（单请求持槽 ≈1s）→ 16 通过 + 8 次 `too many connections`，与 `maxCon=16` 严格吻合 |

**方法学教训（写进守则）**：同址压测里，**探针流量与被测流量共用一个令牌桶**——用探针 ping 测「批准→可拨」会把装置的开销算进产品延迟。判定审批延迟必须读**被测设备自己的日志**，不能读探针的往返。

### 6. 其它已闭环项

- 目录传输私有权限**逐位保留**（0700 不再被放宽成 0755；0750/0600/0400 与目录模式同样逐位保留；修的是二值归一化 + 打包侧硬编码 0755 两个根因）。
- 面板令牌只认 header（查询串形式会进访问日志与 Referer）；离线成员带 `last_seen`（存活与新鲜度是两种事实，分表记）。
- 握手错误不再甩锅给 code（码不一致实际表现为「no partner」）；`--pair-window` 帮助写全真实规则；账号口令旗标两种拼法都接受。
- 发布件：`tools/mkrelease` 组装发布资产（缺 installer 时**明确失败且不留半成品**——此前会留下裸二进制与截断归档，`gzip -t` 还过）；`cmesh verify` 本地校验下载物；README 全部命令可照抄执行（此前 §2 照抄 0/6 通过、§3 四行里三行报 usage）。

### Round 37（2026-11-15 · 6fc00c6 推送 + 复测派单 + 退出码回归修复）

- **交互会话推送**：M4 冲刺全部改动进入远端，`1f64cdc` → **`6fc00c6`**（6 笔：dadbcf7 host policy/--probe/优雅停止、52303df panel 308、c90872e systemd+mkrelease 加固、47206a5 整改文档、c7b57bd CLI credential e2e、6fc00c6 installer 回归）。工作树只剩看板文档与一处修复。
- **新 HEAD 门禁验证**：`go build ./...` PASS；`bash tests/run-tests.sh --quick` **通过 43, 失败 0**（结果段恢复后确认）。
- **发现新 HEAD 真实回归**：`c7b57bd` 提交的 run-tests.sh 丢失结尾"结果汇总 + exit 1/0"段——e2e 失败时退出码不可靠。已在 worktree 修复（恢复结果段），复测通过 43/0 rc=0；**待推送**。
- **复测派单**：四 agent 全部发出 6fc00c6 公告（构建命令 + 各家清单 + 退出码事项申报），四路 ok:true。issue #8 镜像：comment-6900431512。
- 水位推进至 `2026-11-15T18:17:33Z`；oh-no/doubao 复测就绪确认已收，回信 ok:true。

- 水位 `2026-11-15T04:19:45Z` → **`2026-11-15T18:17:33Z`**。水位后 2 条报告：oh-no `18:10:18Z`、doubao `18:17:33Z`，均为复测就绪确认，无缺陷。
- oh-no：环境在线（fetch 至 1f64cdc），确认下一 SHA 复测清单——panel 308/令牌三态/假数据归零/B-3 五项含双向性臂/diagnostics 边界/#7 回归基线；限制沿用（无 root/netns、回环等价形态如实标注）。
- doubao：环境在线，清单对齐；**主动把 M4 冲刺四批次（CLI credential e2e、mkrelease 加固、serve-discovery 优雅停止、Linux 安装/systemd）纳入下轮复测范围**，无 systemd 项将如实申报；headless Chromium 渲染核验可用。
- 两路回信均 ok:true；kuku/netns 未回复本轮就绪催办，继续观察（连续两轮无响应则广播）。
- 批次 C/D 代理仍在运行；工作树 43 条，quick 43 项全绿。

- Hub 水位保持 `2026-11-15T04:19:45Z`，水位后新报告 0 条。四 agent 均超 6 小时静默，已发送"下一 SHA 复测就绪确认"具体催办（panel 308/真实名册/假数据归零/B-3 五项/diagnostics 清单），四路均返回 ok:true。
- 批次 C（serve-discovery 优雅停止）代码落盘：`internal/relay/relay.go`/`relay_test.go` 新增 Shutdown 路径，三个红→绿测试 PASS——`TestShutdownStopsServingReleasesPort`（停止后端口可重绑）、`TestShutdownClosesMembersStream`（members 流受控关闭）、`TestShutdownBeforeListenIsACleanNoOp`（listen 前调用为干净 no-op）。批次代理仍在最终验证，CLI 侧接线由其完成中。
- 批次 D（mclaw Linux systemd 验收）运行中。
- 工作树 43 条；quick 43 项全绿（批次 A）；mkrelease 加固全绿（批次 B）。

- 用户要求继续推进直到 M4 收口。当前 M4 硬阻塞收敛为：①固化候选 SHA（需交互会话放行 commit/push）；②真实 Release 资产 checksum/download/install/rollback；③Linux headless systemd 真实验收；④独立 CLI credential 黑盒 e2e 口径；⑤serve-discovery SIGTERM 优雅停止；⑥mkrelease 输入/资产加固。
- 本轮并行派发四批次（均限定文件范围、红→绿、不 commit/push）：
  - A：tests/run-tests.sh 恢复真实独立 CLI 子进程 credential 一条命令入网验收（逻辑公网 host + NO_PROXY，不放宽生产 host policy）。
  - B：tools/mkrelease 输入版本/平台校验、latest 别名输入拒绝、LICENSE 与 cwd 解耦（自 --installer 向上搜索、go.mod 截止）、显式 --license、归档逐条内容校验。代理在汇报前被并发上限打断，但实现与红测均已落盘；主会话验证 gofmt/vet 干净、`go test ./tools/mkrelease -count=1` 全绿（含 6 个新加固测试）。
  - C：internal/relay + internal/cli/discovery 优雅停止（SIGTERM/SIGINT → Shutdown 超时，端口释放可重启），保持 ListenAndServe 兼容。
  - D：授权主机 mclaw 真实 Linux 验收——构建 1f64cdc、mkrelease 本地组装、自签 CA + 系统信任满足 installer 的 https 要求、独立目录安装、systemd-analyze/enable/restart/kill/自动拉起、healthz smoke；不得干扰现有锚点。
- relay trunk/E2E/resume/LEAVE/revoke-close 仍归 M5/v1.1，不混入 M4 收口口径。


- Hub 水位保持 `2026-11-15T04:19:45Z`，本轮水位后新报告 0 条，agent 最新报告未变化。
- HEAD/origin/develop=`1f64cdc`，文件级工作树 41 条；无 commit/push/merge。
- 门禁事实保持：full Go/race/vet/build/交叉编译 PASS；quick 38 shell + credential Go integration PASS；URL host focused/full discovery PASS。
- 没有新增云端、性能或 relay 数字；Linux systemd、真实 Release/WAN/M5 relay 继续等待外部/实现。

- Hub 水位保持 `2026-11-15T04:19:45Z`，本轮水位后新报告 0 条，四 agent 最新报告未变化。
- 当前 HEAD/origin/develop=`1f64cdc`，文件级工作树 41 条；无 commit/push/merge。
- 当前门禁事实保持：full Go/race/vet/build/交叉编译 PASS；quick 38 shell + credential Go integration PASS；URL host focused/full discovery PASS。
- 本轮没有新增云端、性能或 relay 数字；Linux systemd、真实 Release/WAN/M5 relay 继续等待外部/实现。

- Hub 水位保持 `2026-11-15T04:19:45Z`，本轮水位后新报告 0 条，agent 最新报告未变化。
- 当前候选 HEAD/origin/develop=`1f64cdc`，文件级工作树 41 条；无 commit/push/merge。
- 门禁事实保持：full Go/race/vet/build/交叉编译 PASS；quick 38 shell + credential Go integration PASS；URL host focused/full discovery PASS。
- 当前没有新云端、性能或 relay 数字；继续等待 Linux systemd、真实 Release/WAN/M5 relay 条件，HTML 只更新等待状态。

- Hub 水位保持 `2026-11-15T04:19:45Z`，本轮水位后新报告 0 条；四 agent 最新回报未变化。
- HEAD/origin/develop=`1f64cdc`，文件级工作树 41 条；无 commit/push/merge。
- 当前候选门禁和 focused tests 保持 PASS；quick 为 38 shell + credential Go integration。没有新增云端、性能或 relay 数字，继续等待真实 Linux/Release/WAN/M5 条件。
- 看板只更新当前时间和“等待外部”，不新增虚假开发项。



- Hub 水位保持 `2026-11-15T04:19:45Z`，最新报告时间未变化，本轮水位后新报告数为 0。
- HEAD/origin/develop 仍为 `1f64cdc`，文件级工作树 41 条；没有 commit/push/merge。
- 当前门禁和证据保持：full Go/race/vet/build/交叉编译 PASS；quick 38 shell + credential Go integration PASS；URL host focused/full discovery PASS；真实 Linux/Release/WAN/M5 relay 仍等待外部/实现。
- HTML/KPI 只更新本轮时间和等待状态，无新开发项，不伪造新实验或 agent 回报。

- Hub 水位保持 `2026-11-15T04:19:45Z`，本轮报告总数最新时间仍为 `04:19:45Z`，水位后新报告数为 0。
- 当前候选仍为 HEAD/origin `1f64cdc` + 41 条未提交文件级工作树变更；没有 commit/push/merge。
- 当前门禁事实沿用本轮候选已执行结果：gofmt、vet、build、全量 race、Linux amd64/arm64、Windows amd64 交叉编译 PASS；quick e2e 为 38 shell + credential Go integration PASS，不引用旧 41/41。
- 无新缺陷、无 issue #8 镜像、无新云端/性能数字；HTML/KPI 记录等待外部和当前 M4/M5 缺口，不造开发项。

- 当前 `develop`/`origin/develop`/HEAD 均为 `1f64cdc`，但工作树实际为 41 条文件级状态变更（`git status --short --untracked-files=all`）；当前 worktree 不是外部可复测固化 SHA。
- 当前门禁：全量 Go race、vet、build、Linux amd64/arm64、Windows amd64 交叉编译均 PASS；quick e2e 为 38 个 shell 场景 PASS + credential Go integration PASS。不能继续把历史 41/41 记录当作当前 worktree 证据。
- M4/M5 事实未改变：B-1/B-2/B-3、panel、status/ping/bench、`@cat` 有 `1f64cdc` 外部/云端证据；真实 Release/Linux systemd、mesh relay-only、mesh E2E、resume、LEAVE/revoke-close、平台真机仍未完成。
- URL host 安全修复与 discovery/CLI focused tests PASS；成员 endpoint 私有语义保留。mclaw loopback/direct 和 netns client→anchor 证据保留，公网/WSS/跨云 NAT/反向纯出站仍未验证。

- 当前候选 worktree 实际 `git status --short --untracked-files=all` 为 41 条状态变更；HEAD/origin/develop 仍为 `1f64cdc`。此前文档中的 18 项统计已过时，已统一改为 41 条。
- 当前门禁重跑结果：`gofmt -l .`、`go vet ./...`、`go build ./...`、`go test ./... -race -count=1`、Linux amd64/arm64 与 Windows amd64 交叉编译全部 PASS。
- `bash tests/run-tests.sh --quick` 当前通过 38 个 shell 场景；credential enrollment 由独立 `go test ./internal/cli -run '^TestJoinWithCredentialAdmitsDevice$'` 通过。脚本已不再直接用 loopback URL 绕过生产 host policy，但该重构改变了计数口径，未把它写成旧版 41/41。
- URL host 安全策略：discovery base URL 拒绝 localhost/环回/私有/保留地址；成员 endpoint 私有语义保留；discovery 全包和 CLI focused tests PASS。
- 真实云端证据：mclaw loopback/direct 双向业务通过，netns/SNAT client→anchor 通过；公网/WSS/跨云 NAT/anchor→纯出站成功仍未验证。

- 云端 mclaw 验收发现 discovery base URL 可接受 localhost/环回/私有/保留地址，属于真实 URL host 安全缺陷。
- 修复：`normalizeBaseURL` 增加 host/port 和地址分类拒绝；成员 endpoint 的私有/环回语义保持，因为它们是 mesh/NAT 公告数据。discovery 测试使用逻辑公网 host + HTTP/WebSocket test transport 路由本地 httptest；`go test ./internal/discovery -count=1` PASS；CLI credential/join focused tests PASS。
- `tests/run-tests.sh --quick` 的独立二进制 credential 段仍直接用 loopback discovery URL，3 项失败；这是测试 harness 与新安全策略冲突，不回退生产策略，不算云端 B-3 回归。下一步应把 shell harness 改成授权测试 host/transport 或使用云端 fixture。
- 安装代理限定目录测试保持 PASS：installer lifecycle、HTTPS-only fake-curl、mkrelease staging/upgrade tests 通过；真实 Linux systemd/reboot 仍待外部。

- 第二轮代理只读完成，主 worktree 未修改、未 commit/push/merge；环境 macOS arm64 Apple M4、Go 1.27.2，结果保存在 `/tmp/container-mesh-perf/round2/`。
- 64KiB frame microbench：当前 `readBenchFrame` 69,782B/5 alloc、5.56–5.67µs/op；临时复用读取 4,245B/4 alloc、1.71–1.83µs/op；真实 mesh QUIC 64MiB 双向 profile：累计分配 328.11MB→54.38MB（-83.4%），RSS 20,768→20,032KB（-3.5%），CPU profile 无稳定收益。
- relay 复测：64MiB 吞吐 253.35MiB/s、峰值 RSS 17,792KB、平均 CPU 9.27%、峰值 27.20%；128MiB 吞吐 272.88MiB/s、峰值 RSS 17,968KB、平均 CPU 20.34%、峰值 44.00%；文件校验和 send/recv 退出码均通过。
- `p2pquic.readLoop` 2MiB profile 分配约 2,086KiB、峰值 RSS 18,400KB；不能直接 `sync.Pool`，因为 `Recv` 将 slice 所有权交给调用方，池化会有数据覆盖风险。正式优化前需 Release/ownership API、慢消费者/关闭竞态/长时多消息测试。
- 处置结论：frame buffer 复用是明确 alloc 优化点，但未证明 CPU/吞吐收益，临时变体不合入；公网/WAN/Linux/WSS/丢包/高并发均未验证。

- Hub 水位 `2026-11-15T03:31:24Z` 后新增 oh-no `03:56:03Z` 和 doubao 回执 `04:19:45Z`；两条均无代码修改。
- oh-no：B-3、diagnostics、@cat、面板全 PASS；B-4 候选为锚点 `serve-discovery` state 没有 `discovery.json`，锚点主动 `mesh ping/msg` 返回 config not found。成员→锚点、成员↔成员均 PASS；本轮按状态边界记录，等待产品确认，不判回归。
- 处置：oh-no 回信 `ok:true`；kuku 超过 6 小时，具体催办 `ok:true`；无真缺陷、无 issue #8 镜像。
- 性能第二轮已派发独立代理，目标为 bench frame、p2pquic read buffer、relay 负载前后 profiling；当前等待真实命令、样本和前后数字，未提前写优化完成。

- 独立性能代理完成只读研究，未修改仓库、未 commit/push/merge；环境为 macOS Darwin 24.6 arm64、Apple M4、Go 1.27.2，未访问公网 Hub 或读取 token。
- 实验命令/结果已保存在 `/tmp/container-mesh-perf/`：direct QUIC `p2pquic` 2MiB 单向 8 次为 137.7–235.2MiB/s；完整 `mesh.RunBench` mTLS/member pinning、3MiB 双向 10/10，每次约 30–40ms，单向折算约 75–100MiB/s；旧 p2p2 无丢包 288–306MB/s、5% 丢包 55–65MB/s。
- WebSocket：32KiB `SendRecv` 约 1.78GB/s、约 161.5KB/op/32 allocs/op；服务端 `RecvInto` 6.36–6.49GB/s、1–2 allocs/op。discovery httptest loopback 注册+首 snapshot 50 次约 10ms。
- 旧阶段 1 relay 真实临时 `cmesh relay` + `send/recv`、文件校验均通过：16MiB 135.3MiB/s、峰值 RSS 16,240KB、平均/峰值 CPU 4.75%/8.8%；64MiB 220.2MiB/s、17,216KB、9.37%/27.6%；128MiB 263.9MiB/s、17,264KB、17.36%/46.8%。这些是 macOS loopback，不代表公网/WSS/NAT/Linux。
- profile 假设：relay CPU 主要在双向 WebSocket read/write 和 syscall；mesh bench `readBenchFrame`/`sendBenchData` 占主要 alloc profile；`p2pquic.readLoop` 每消息 `make([]byte,n)` 是 direct message 分配热点。
- 优化队列：P0 先完成 mesh relay trunk/direct fallback；P1 bench frame buffer 复用与 p2pquic 受限 buffer pool；P2 relay 32/64/128/512KiB 写路径对比；另做 Linux/WSS/443/真实 NAT 和 `/proc`+`smaps_rollup` 峰值 RSS 对照。未测公网、WSS/443、丢包/延迟注入、bench 峰值 RSS，不写成已完成。



- 用户要求值守不能停在报告轮询：从本轮起，值守自动化每轮同时运行两条线：Hub/缺陷/门禁主线，以及独立性能研究线。
- 性能研究代理首轮任务：direct QUIC、discovery WebSocket、阶段 1 relay send/recv、未来 mesh relay trunk；实验必须给出拓扑、真实命令、样本数、CPU/RSS、吞吐、RTT、丢包、重连、分配和优化建议，严格区分 loopback 上界、模拟 NAT 与真实公网授权环境。
- 访问现有公网服务的安全边界已写入自动化：只允许 http/https；请求前校验 host，拒绝 localhost、环回、私有和保留地址；凭据只从环境变量或密钥服务读取，不写入源码、示例、测试、文档或日志。
- 当前首轮研究仍在运行，尚无性能数字或优化结论；HTML/KPI 标为“性能研究进行中/等待实验结果”，不伪造完成状态。

- 水位 `2026-11-14T17:31:14Z` 后新增 2 条报告：doubao `2026-11-15T03:28:12Z`、netns `2026-11-15T03:31:24Z`。两份均来自 `1f64cdc`，没有代码修改、没有 commit。
- doubao：`join --cred`、status、client→anchor `mesh ping` 10/10、成员双向基线均 PASS；iptables DROP 后 anchor/member→纯出站 client 仍 `all endpoint(s) fail`，确认是 mesh relay fallback 缺口，不是回归。
- netns：真 SNAT/netns 环境确认 B-3 五项 `status/ping/msg/send-to/run` 全部 rc=0，client→anchor `mesh ping` 30/30、p50 0.265ms，bench 20MB/50MB 双向字节完整；anchor→client `network is unreachable`，仍归 v1.1 mesh relay trunk。
- netns 资源采样：join 常驻进程 20 个 `/proc` 采样点，前 10 秒含一次 50MB bench；VmRSS `15,620–15,800 KB`，超过 15MB 目标；CPU 约 0.34s/20s（约 1.7% 单核时基）。bench 一次性峰值 RSS 未采样，资源 KPI 记为**未达标/待 profiling**，不把空闲 relay 基线代替业务负载。
- 处置：新报告 2 条，均判 PASS/已知缺口，无真缺陷、无红→绿修复、无 issue #8 镜像；doubao/netns 回信均 `{"ok":true}`。oh-no/kuku 已超过 6 小时，本轮具体催办均 `{"ok":true}`。水位推进到 `2026-11-15T03:31:24Z`。



- Hub 水位保持 `2026-11-14T17:31:14Z`，本轮读取到的最新时间仍为 `17:31:14Z`，水位后新报告数为 0。
- 四 agent 最新报告：doubao `17:27:02Z`、kuku `17:31:14Z`、oh-no `16:41:17Z`、netns `09:56:04Z`。按当前 UTC `2026-11-15T03:12:23Z` 计算，netns 已静默约 17 小时，其他三家约 10 小时以上；本轮已向四个 inbox 发送具体复测任务/广播，四路均返回 `{"ok":true}`。下一轮若仍无 netns 回报，提示用户唤醒。
- 本轮无新开发缺口，无需代码复现、回信或 issue 镜像；看板、DEV-KPI、PROJECT-PLAN 已记录“等待外部”，未修改水位。
- 当前 HEAD/origin/develop 仍为 `1f64cdc`，工作树 18 项；已有门禁证据仍是 18:53Z 的全绿批次，值守会话不重复跑无代码变化的完整 CI，也不 commit/push/merge。



- 完成提示词到证据的逐项审计：`status`、`mesh ping`、`bench` 的直连路径、`doctor` 复用 probe、阶段 1 relay `send/recv`、R-3.2～R-3.6、无 UDP endpoint 的 relay 验收、CPU/RSS 资源指标均逐项核对。
- 新增 `cmesh status --probe <deviceID>`：只在用户显式要求时建立一次真实成员证书 pinning 的 direct QUIC ping；成功输出 `reachability=direct`，失败输出 `reachability=degraded` 和 `probe_error`。默认 status 仍为 800ms snapshot、无 transport 证据时 `unknown`。
- 旧 relay 路径已确认不能充当 mesh relay：`send/recv` 使用共享 code + A/B room 配对，discovery `/mesh/*` 仍是控制面；没有 device-ID 路由、成员认证 trunk、端到端 relay probe 或 relay bench。纯出站 NAT 的 anchor→client 失败仍归 R-3.2/v1.1 缺口。
- 资源基线：本机 macOS 上 `go build -trimpath -o /tmp/cmesh-resource-gate ./cmd/cm` 后运行 `cmesh relay --addr 127.0.0.1:0`，5 次 `ps` 采样 RSS 6,256–9,024 KB、CPU 0.0%（启动瞬时 2.1%）；这是空闲 relay 基线，不是 mesh relay 负载 KPI PASS。CPU≤8% / RSS≤15MB 的业务负载验收仍待定义统一拓扑、采样窗口和消息/bench 工作负载。
- 本轮 CLI/mesh 单测和 `go build ./...` 通过；完整 CI、relay vertical slice 和新 SHA 外部复测仍未完成。

- Hub 水位保持 `2026-11-14T17:31:14Z`；本轮没有晚于水位的新报告。四 agent 最新时间：doubao `17:27:02Z`、kuku `17:31:14Z`、oh-no `16:41:17Z`、netns `09:56:04Z`。netns 距本轮约 9 小时，已发送具体 NAT/新 SHA 催办，Hub 返回 `{"ok":true}`；其他三家未超过 6 小时，不重复催办。
- `1f64cdc` 外部复测结论保持：status/ping/bench、B-3、面板、双向性和 `@cat` 均 PASS。doubao 报告中的 D-1 聚合未采集假零、kuku 报告涉及的恢复 flaky 测试均已有本地修复；本轮不重复改代码、不扩大旧报告处置范围。
- 本地门禁于 `2026-11-14T18:53Z` 全绿：`gofmt -l internal/ cmd/ tools/` 无输出、`go vet ./...`、`go build ./...`、`go test ./... -race -count=1`、`bash tests/run-tests.sh --quick`（41/41）、Linux amd64/arm64 与 Windows amd64 交叉构建均 PASS。
- HTML、DEV-KPI、PROJECT-PLAN、GPM 已同步远端 tip `1f64cdc`、本地待放行 18 项、Hub 水位和 agent 最新报告时间。值守会话不 commit/push/merge；D-1 与两项 flaky 测试修复标为“已就绪待放行”。

（2026-11-14 · diagnostics follow-up）

- oh-no 在 `1f64cdc` 复测发现 `--count 1000 --interval 100ms` 被默认 20s deadline 截在 196/1000；这不是传输失败，而是命令契约冲突：最大包数×最小间隔需要超过默认 timeout。修复：默认 timeout 按 discovery 15s + sample interval window + per-reply timeout 动态计算；显式 `--timeout` 仍由用户控制。新增 CLI 边界测试。
- doubao 在 `c530c7d` 复测发现聚合层把缺失遥测折成 `0.0 MB/s / 0.00 TB`；修复：任一成员字段缺失时聚合显示 `未采集`，不得把缺失解释成测得零。
- kuku 报 `TestStreamRecoveryFiresImmediateAnnounce` 5/8 flaky；修复：等待 `Client.Joined()` 和 kill 前 register 基线，再断流，断言 kill 后 register 增量，`-count=20` 全绿。


- doubao 在 `c530c7d` 面板复测发现：设备卡片显示 `未采集`，但全局聚合卡片把缺失遥测用 `|| 0` 汇总成 `0.0 MB/s` / `0.00 TB`。修复：聚合只有在成员字段真实存在时才计算；否则显示 `未采集`，不把缺失解释为实测零。
- kuku 发现 `TestStreamRecoveryFiresImmediateAnnounce` 5/8 复现失败，失败等待「pre-join fast tick drained」；该等待把调度时序当成产品事件。修复：先等待 verified snapshot 使 `Client.Joined()` 为 true，记录 kill 前 register 基线，再断流，断言 kill 后 register 严格增加且最多增加一次。`-count=20` 全绿。
- netns 纯 NAT 复测：client→anchor 10/10，anchor→纯出站 client 失败；这是无 DNAT/无中继回退的 v1.1 网络缺口，不是凭证入网缺陷。


**催办目标**：实现 `status`、ping 采样约束和 `bench`，同时保持不依赖 Cockpit、不抢业务流、不伪造 relay/retransmission 证据。

### 已落地（本地 worktree，待推送）

- `cmesh status`：snapshot 等待预算默认 **800ms**；文本/JSON 输出成员、版本、online、端点、last_seen、reachability。没有 transport probe 证据时输出 `unknown`，不把端点候选写成直连，不把 relay 写成已连接。
- `cmesh mesh ping`：`--interval` 最小 **100ms**，`--count` 限制 **1..1000**；超时回复计为 loss 并继续采样；输出 sent/received/lost、丢包率、p50/p95 和 direct/degraded/unknown。兼容保留 `PingExchange`，新增 `PingExchangeStats`。
- `cmesh bench <peer-id>`：默认每方向 **100MB**，独立 `KindBench` QUIC stream，双向并发、`--rate` 每方向限速、帧边界和结束/统计帧校验，不写业务文件、不占用消息/文件/forward 流；QUIC 的逐流 retransmission counter 当前不可观测，命令明确输出 `retransmissions=n/a`，不伪造为 0。
- `@cat` 大输出：ACK 读取不再用 stream 级 `io.LimitReader(4097)`，新增 4KiB+真实 run frame/exit frame 回归测试，修复前稳定 `unexpected EOF`。

### 证据与边界

- 本地 focused tests：`go test ./internal/cli ./internal/mesh -count=1` PASS。
- 完整 Go tests、race、e2e 41/41、Linux amd64/arm64、Windows amd64 交叉编译已在本轮通过。
- netns 真 NAT：凭证入网 0.857/0.851s，client→anchor 10/10；anchor→纯出站 client 失败，因为无 DNAT 且现有直连命令无中继回退。该结论归 v1.1 中继路由/路径回退，不归因 B-3。
- CPU≤8%/内存≤15MB 尚无统一采样定义和端到端阈值测试，暂不标 PASS；下一步需定义 RSS/Heap、采样窗口和端点分离后再测。


### 1. 缺陷闭环（agent 抓到，协调者修复）

| 编号 | 缺陷 | 根因 | 修复 | 复测 |
|---|---|---|---|---|
| B-1 | 委派成员能入网、在名册、**谁都拨不通** | `SnapshotAdapter.KeyFor` 只按账号公钥验签；凭证证书由委派者签 | 用快照携带的委派记录验签；无匹配委派仍拒 | oh-no：anchor→D9 10/10（p50 0.393ms）、D9→E9 10/10 |
| B-2 | `--endpoint host:0` 被原样公告、锚点判畸形 | `--listen 0` 会展开而 `--endpoint` 不会 | `resolvePortZeroEndpoints` 按监听器实际端口解析（纯函数+测试） | oh-no：stderr 精确提示 + 名册 online + 10/10 可拨 |
| B-3 | 凭证设备第二条命令起全废（`saved invite is malformed`） | `join --cred` 把凭证串存 `discovery.json.invite`，meshcmd 无条件 `DecodeInvite`（只认 `cmesh://join?`） | 配置改存派生字段（account_id/account_pub/discovery_addrs）；`cfg.invite()` 优先字段、旧格式回退解码 | 协调者真实二进制：入网→ping 5/5 p50 0.531ms→msg delivered 1；oh-no 待新 SHA 复跑三步+双向臂 |
| — | 面板白屏（启动横幅的 `/panel` 无尾斜杠） | RFC 3986：文档在 `/panel` 时 base 是 `/`，`./assets/x` → `/assets/x` → 404 | `/panel` → 308 → `/panel/`（带 `Cache-Control: no-store`，防缓存毒化后续验证） | 协调者实测 308→200，JS/CSS 200；doubao 新 SHA 复测（黑盒口径已定） |
| — | **面板默认显示两台不存在的设备** | 原型「香港3机智能聚合」开关**默认开启**，append 两台合成设备（伪造坐标/端点/运行时/online）+ 1.5s 定时器抖动假遥测 | 删设备/开关/连线弧/假遥测表/抖动循环/编造国旗发行版/虚构拓扑硬编码弧线；「失败后谎报已发起探测」改为如实报错；缺数据显示**未采集** | doubao：九串产物+源码+二进制 `strings` 全 0 命中；3D 设备数==members；40 个按钮无开关 |

### 2. 验收工具本身的缺陷（与被测缺陷同样重要）

| 工具错误 | 后果 | 规则 |
|---|---|---|
| `TestCockpitAssetsLoadThroughTheMount` 手工拼 `"/panel/"+ref` | 断言了写测试者的假设，不是浏览器行为；白屏存在时测试全绿 | **验证浏览器行为必须用 URL 解析器**（`ResolveReference`/`urljoin`），不得手工拼接路径；与第 12 轮 §5「探针共享令牌桶」同根 |
| 协调者的真实锚点验证脚本与测试犯同一个错 | 两道绿灯互相印证、双双无效 | **两个共用同一错误假设的绿灯不构成交叉验证** |
| 红测第一轮只把关键词包进括号，子串仍在 | 测试照绿＝无效红测 | 见 §3 |
| canvas `fillText` 画的内容 DOM 查不到 | 「像素里有」≠「可核验」 | 给 canvas 挂 `sr-only` 文本镜像 + `role="img"`/`aria-label`（doubao 建议，已落地并通过 DOM 断言验收） |

### 3. 方法学：红测的三条规则（doubao，round 60 回执）

1. **变异必须是语义删除**——删词/删整句/改指向；包裹、加前缀、改大小写（除非被测逻辑确实大小写敏感）不算。本轮实例：把免责声明句改成同义句 → 测试红且报文精确；只把关键词包进括号 → 子串仍在 → 测试照绿（无效红测）。
2. **红文必须匹配预期的失败原因**——不止"红了"：要确认红文正是那条断言（如 `demo-content exemption has lost its cover`），而不是编译失败、nil panic 或另一个断言先炸。
3. **恢复后同一次运行内绿**——不能靠"上次是绿的"。

同根结论（与 §2、第 12 轮 §5 并列）：**任何验证装置都要先证明它测的是被测物。**

### 4. 其它

- `anchor-hk-01` 特意不入演示黑名单：它只活在设计规范页的免责声明句里；黑名单化会逼测试删免责声明，方向反了。配套 `TestCockpitDesignSpecCarriesItsDisclaimer`：bundle 中凡出现该名处必须同时存在免责句（防豁免变成无人看守的门）；doubao 指出字节共现 ≠ 界面可见，DOM 侧黑盒断言由其下轮补（分工：Go 侧管构建产物，黑盒管渲染）。
  - doubao 追加（round 60 收尾）：**DOM 共现 ≠ 读表人可见**——免责句落在页脚或总则，`body.innerText` 一样命中。三方收敛为**作用域断言**：免责句与被免责内容必须同区块；协调者已给 06 区块挂 `data-testid="spec-state-matrix-06"`（doubao 按此定位，文本定位做后备）。
  - doubao 黑盒基线（`24bbf2f`，headless 实测）：设计规范 06 表是**五台假设备的完整名册矩阵**（编造名册版本 v42/v43、四态俱全），与同屏真实数据（1 台、v1）直接矛盾，且同页立着「数据诚信铁律」——免责缺失时这张表就是伪装成名册的示意。协调者已删全部假版本号（`In Roster（示意）`），免责句（worktree）落在 06 区块内。
- 308 重定向显式 `no-store`：缓存过的 308 会让「撤掉重定向看红」读出假数据（doubao）。
- 308 测试必须用显式 no-follow 客户端：Go 默认客户端会跟随 308，断言 308 会红、断言最终 200 则什么都没测（doubao；实现从第一版即是，价值在把理由写进注释）。
- 操作坑：`show-pub` 打印**设备**公钥，`cred create` 凭证验签要**账号**公钥（`account show` 的 pubkey.b64/hex）——oh-no 实踩，记入部署文档待办。
- 308 会被浏览器缓存，`no-store` 只落在转发响应上（同上 doubao ③）。

### 5. 协调

- kuku（28.5h）/ netns-probe（28.2h）静默：四次触达（三次催办+一次广播）无响应 → **最终通知已发**，48h（约 11-16 04:30Z）不回按失联结案、任务永久改派。netns 的 429 项已由 doubao 闭环（第 12 轮 §5）。
- `24bbf2f` 双环境复测全 PASS（见 §1 复测列）；工作树另有一批未推修复（B-3/308/假遥测/文档），等网主放行后公告新 SHA，doubao 按 ①②③ 口径 + 字节差值核对（期望 −3,847 / −507 B）复测。

### Round 37（2026-11-15 · 交互会话：M4 双硬阻塞真机验收）

- **推送**：develop `1f64cdc → 6fc00c6`（6 个逻辑提交：host policy/status probe/relay Shutdown、面板 308+新资产、systemd unit 资产、e2e 43 真实 CLI 化、安装器离线回归、文档）→ `6f07562`（口径同步）。身份 muzimu217 author+committer 双查通过（网主纠正后本地 override）。门禁：race 18 包、e2e 43/43、交叉编译全过。
- **Release 验收**：prerelease `v1.0.0-rc1`（17 资产：5 平台二进制+tar.gz+latest 别名+安装器+SHA256SUMS；prerelease 不占 latest 正式位）。
- **VPS 真机验收（175，OpenCloudOS 9.4，隔离目录+高位端口 39771，不碰生产 anchor/hub/nginx）**：16/17 PASS——A 真实下载/校验/安装 ✓；C systemd 启动/healthz/kill-9 Restart=always 自动拉起(PID 212244→212312)/SIGTERM 优雅停止(Shutdown 路径)/端口释放 ✓；D 重装替换轮换+current 回滚 ✓；E 卸载保留状态 ✓；F 清理 ✓。
- **认账**：B 篡改用例 FAIL 为测试脚本设计错误（篡改本地旧文件而非下载源，安装器重新下载远端资产当然通过）——产品门禁证据=A 项 SHA256 一致+离线回归 fixture 篡改路径；不算产品缺陷，不悄悄改绿。
- **未验收（如实）**：reboot 恢复（需重启生产服务器，待网主授权或另找 VM）；darwin/windows 归档资产后台上传中（10/17 已上）。
- **派发**：hub channel+4 inbox 五路 ok:true（任务书同 SHA=6f07562）；issue #8 mirror=comment-6900352297。systemd/Release 项在任务书中标注环境阻塞（容器 agent 不可测）。

### Round 38（2026-11-15 · 值守：复测报告回灌 + F-1 修复）

- 水位推进：`18:17:33Z` → **`20:26:52Z`**。水位后 4 条消息：协调者任务书（@6f07562）、oh-no 6fc00c6 复测报告、doubao 6f07562 复测报告、协调者双硬阻塞验收公告。
- **oh-no（6fc00c6）全项 PASS**：面板黑盒（308/no-store/令牌三态/假数据归零/离线保留）、B-3 五动词含凭证→凭证双向臂（10/10）、B-4 边界、`@cat` 六档、diagnostics 含 `--count 1000` 全跑（1000/1000）。操作坑登记：`cred create` 无 `-state` 绑定全局账号。
- **doubao（6f07562）**：panel 线全 PASS（3D 数量==members、A1–A5、D-1「未采集」修复渲染核验）；mkrelease 正反跑 PASS；**发现 F-1**：`packaging/install_test.sh` fixture 硬编码 darwin-arm64，linux/amd64 必 rc=1（测试装置缺陷，产品本体经仓库外同构 harness 7/7 验证无恙）。
- **F-1 已修复（worktree 待推送）**：fixture 按 uname 解析 os/arch（与安装器同逻辑），macOS darwin/arm64 复测绿；待推送后 doubao 在 linux 复跑确认。
- 回信：oh-no/doubao 确认 ok:true；kuku/netns 具体催办（任务书项：e2e 43 场景+优雅停止 / NAT 方向性+RSS 基线）ok:true。issue #8 镜像：comment-6901190655。
- **M4 剩余**：kuku/netns 复测回报；reboot 验收（待授权）；F-1+run-tests.sh 结果段推送（worktree 就绪待放行）；darwin/windows 归档上传完成。

### Round 39（2026-11-15 · 值守：F-1 推送状态确认 + kuku/netns 广播）

- 水位推进：`20:26:52Z` → **`21:18:15Z`**。水位后 1 条：doubao F-1 复跑回报（@180b31c）。
- **doubao 证实**：F-1 修复（install_test.sh 按 uname 生成 fixture）与 run-tests.sh 结果段**均仍在值守 worktree，未推送**——180b31c 为 docs 提交，install_test.sh 最后改动仍是 6fc00c6；linux/amd64 复跑 rc=1 与预期一致，产品本体 7/7 结论不变。修复推送后我将公告，doubao 即刻复跑确认。
- 广播：kuku（超 28h）/netns（超 18h）连续多轮无响应，已按规则向两 inbox 广播待办明细（kuku：43 场景+优雅停止三项；netns：NAT 方向性+RSS 基线对照），并提示网主唤醒。三路投递均 ok:true。
- 水位 → `21:18:15Z`。

### Round 40（2026-11-15 · 值守：静默等待轮）

- 水位保持 `21:18:15Z`，水位后新报告 0 条；远端 tip 仍为 `180b31c`，两笔修复（F-1 + 结果段）仍在 worktree 待放行。
- kuku/netns 广播后暂无响应，继续等待；doubao 待新 SHA 公告后复跑离线回归；oh-no 报告已闭环。
- 无新开发项、无新实验；看板只更新等待状态。

### Round 41（2026-11-15 · 值守：静默等待轮 · 二次提示唤醒）

- 水位保持 `21:18:15Z`，水位后新报告 0 条；远端 tip 仍 `180b31c`，两笔修复（F-1 + 结果段）仍在 worktree 待放行。
- kuku（超 30h）/netns（超 20h）：广播后连续第二轮无响应，按规则再次提示网主手动唤醒或确认失联结案改派。
- doubao 待新 SHA 公告后复跑；oh-no 已闭环；无新实验。看板只更新等待状态。

### Round 42（2026-11-15 · 值守：静默等待轮）

- 水位保持 `21:18:15Z`，水位后新报告 0 条；远端 tip 仍 `180b31c`，两笔修复仍在 worktree 待放行。
- kuku/netns 连续第三轮无响应（已广播+两次提示网主）；doubao 待推送公告；oh-no 闭环。无新实验，看板只更新等待状态。

### Round 43（2026-11-15 · 值守：静默等待轮）

- 水位保持 `21:18:15Z`，水位后新报告 0 条；远端 tip 仍 `180b31c`，两笔修复（F-1 + 结果段）仍在 worktree 待放行。
- kuku/netns 连续第四轮无响应（待网主唤醒或失联结案）；doubao 待推送公告；oh-no 闭环；无新实验。看板只更新等待状态。

### Round 44（2026-11-16 · 值守：netns 第五十四轮复测回灌）

- 水位推进：`21:18:15Z` → **`2026-11-16T02:13:52Z`**。水位后 1 条：netns-probe 第五十四轮（@6f07562）。
- 结果：ENROLL 0.26s（名册 v3 无人工审批）；client→anchor `mesh ping` 30/30（p50 0.319ms p95 0.458ms，link=direct）；anchor→纯出站 client 反向 `network is unreachable`——R-3.2 边界第三轮同形，不判缺陷。
- **RSS 五段采样新观测**：50MB bench 期间爬升至 ~139MB 峰值后 GC 回落，稳定态 15,992–16,296KB——**超 15MB 目标，维持「未达标」口径**；与上轮 15.62–15.80MB 基线同量级、无数量级回归（上轮采样窗口在爬升前，口径差异已写明，不作回归证据）。代理未做 heap profile，不判泄漏，分配剖析留给评审决定。
- **测试适配说明（将写入测试文档）**：生产 host policy 拒绝私网 IP 字面量，netns/私网拓扑须用逻辑主机名（/etc/hosts + nip.io 形态）才能入网——这是按设计的正确行为，不是缺陷。
- 吞吐仅上界参考（veth/bridge）：20MB 191.66MiB/s、50MB 94.88–147.29MiB/s，`retransmissions=n/a`。
- 回信 netns ok:true；水位 → `02:13:52Z`。

### Round 45（2026-11-16 · 值守：kuku 第五十五轮回灌 + K-1 修复）

- 水位推进：`02:13:52Z` → **`02:54:19Z`**。水位后 1 条：kuku 第五十五轮（@6f07562），恢复在线。
- **kuku 复测结果**：回环绑定 PASS（0100007F:46A0）、审批→可拨 1.22s、ping 10/10（p50 0.265ms）、多行中文消息 105 字节逐字节一致（补上第 51 轮证据缺口）、1MiB send-to 双端 sha256 一致、run 四动词 rc=0、名册保留离线成员；43 场景 42✓/1✗（mkfifo EPERM 前提不成立作废；SUITE_EXIT=0 与已知结果段缺失一致）。
- **发现 K-1（真实缺陷候选，已确认并修复）**：`cmesh relay` SIGTERM 未接线优雅停止——退出码 143、6ms 硬死、活跃会话 close 1006、无 shutdown 日志；库层 relay.Server.Shutdown 已实现有测试但 CLI 未接线。kuku 定位准确到 internal/cli/relay.go:55 仅 ListenAndServe。
- **K-1 修复（worktree 待推送）**：runRelay 按 discovery.go 同款 signal.NotifyContext(SIGINT,SIGTERM)+srv.Shutdown(10s) 接线，信号注册先于 ListenAndServe。本机红→绿：修复前 exit=143，修复后 SIGTERM **exit=0**。gofmt/vet 干净，cli+relay 全测 PASS。
- **观察项采纳**：invite 生成侧未拦截 host policy 会拒的地址——挂档改进项。
- 回信 kuku ok:true；水位 → `02:54:19Z`。
- **M4 剩余**：两笔修复+K-1 三项推送（worktree 就绪待放行）；doubao/kuku 推送后复跑确认；reboot 验收待授权；darwin/windows 归档上传中。

### Round 46（2026-11-16 · 值守：全量门禁复核 + 推送前就绪）

- 用户曾放行推送但随即中断本轮；按值守规则本轮不执行 commit/push/merge，改为完成推送前全部准备，标「已就绪待放行」。
- 清理意外构建产物 `cm`（工作树根目录遗留）。
- 全量门禁复核（当前 worktree）：`gofmt -l .` 无输出、`go vet ./...` PASS、`go build ./...` PASS、`go test ./... -race -count=1` 18 包全 ok、`bash tests/run-tests.sh --quick` **通过 43, 失败 0**（rc=0，含恢复后的结果段）、Linux amd64/arm64 + Windows amd64 交叉构建 PASS。
- 提交身份本地配置核对：muzimu217 / 1278844978@qq.com ✓。
- **建议提交拆分（4 笔，供交互会话执行；.atomcode/ 与临时产物不入库）**：
  1. `fix(relay): wire SIGTERM/SIGINT graceful shutdown into cmesh relay` — internal/cli/relay.go（K-1，kuku round 55 发现）
  2. `fix(packaging): resolve installer test fixture platform via uname` — packaging/install_test.sh（F-1，doubao linux rc=1 实测）
  3. `test(e2e): restore result summary and exit-code contract` — tests/run-tests.sh
  4. `docs: sync watch rounds 38-46 status` — docs/DEV-KPI.md、docs/ROUND-LOG.md、docs/project-dashboard.html
- 每笔提交前建议重跑对应范围测试；推送后公告新 SHA，doubao（linux 离线回归复跑）、kuku（relay SIGTERM 退出码 0 验收）即刻复跑。

### Round 47（2026-11-16 · 交互会话放行：修复批推送 77c14b5）

- 用户明确放行推送并授权全权执行；同时要求停止两个长时间运行代理——已分别要求其停止改动并汇报终态：serve-discovery 优雅停止代理确认其改动早已进入 HEAD（磁盘与实现一致，含变异检查证据），mclaw 验收代理确认远端已清理干净（unit/CA/用户/实验目录全删，长期锚点未受影响）。
- **推送前门禁全绿**：gofmt/vet/build PASS；`go test ./... -race -count=1` 18 包全 ok；`tests/run-tests.sh --quick` 通过 43/失败 0；Linux amd64/arm64 + Windows amd64 交叉构建 PASS。
- **4 笔拆分提交并推送 develop**：`18404f1` fix(relay) K-1 / `537a107` fix(packaging) F-1 / `7c493ab` test(e2e) 退出码契约 / `77c14b5` docs。`180b31c..77c14b5`。身份 muzimu217。推送首次因代理 TLS 失败（SSL_ERROR_SYSCALL），按既有规避方式重试成功。
- **复测派单**：四路 inbox 公告 77c14b5 与各自复测项，全部 ok:true。issue #8 mirror=comment-6911892755。
- M4 剩余：四 agent 同 SHA 复测回报；reboot 验收待授权；darwin/windows 归档上传完成。

### Round 48（2026-11-16 · 值守：F-1 复测通过 + 三线催办）

- 水位推进：`02:54:19Z` → **`09:18:19Z`**。水位后 1 条：doubao F-1 修复复测通过（@8c84e73）。
- **doubao（linux/amd64）**：离线安装回归 **rc=0**，资产名 `cmesh-latest-linux-amd64` 正确生成、SHA256 校验、uninstall/purge 全绿（"packaging install tests: ok"）；与 6f07562/180b31c 的 rc=1 形成对照，**F-1 闭环**。install 线至此全绿（产品本体同构 harness 7/7 + 离线回归 linux 绿灯）。
- 复测矩阵现状：oh-no ✅（6fc00c6 全项 PASS）、doubao ✅（panel/mkrelease/F-1 全闭环）、netns ✅（第五十四轮入网 0.26s/ping 30/30）、kuku ✅（第五十五轮回环零回退 + 发现 K-1）。
- 剩余待回报：kuku（relay SIGTERM exit 0 + quick 43/0 rc 契约）、oh-no（8c84e73 无回退复核）、netns（8c84e73 NAT/RSS 基线）——已发具体催办，三路 ok:true。
- 回信 doubao 登记 ok:true；水位 → `09:18:19Z`。

### Round 49（2026-11-16 · 值守：oh-no 全项 PASS + 本地开发批次 + 广播催办）

**Hub 处置**
- 水位推进：`09:18:19Z` → **`09:47:24Z`**。水位后 1 条：oh-no 对 `8c84e73` 的复核。
- **oh-no-agent ✅**：`8c84e73` 上 panel / B-3 五动词+双向臂 / diagnostics / B-4 边界**全项 PASS，无回退**。面板黑盒 401/401/200/401/308/no-store 全部符合，离线成员保留名册并带 `last_seen`，下线跟随 0.10s、恢复 0.61s；B-3 双向臂（凭证↔凭证 10/10 p50 1.069ms、凭证→锚点 10/10 p50 0.306ms）link=direct；`status` 无 probe 时 `reachability=unknown` 不伪造；`@cat` 4095/4096/4097/65536/1048576B 字节精确、远端退出码透传 rc=1 正确。回信 ok:true。
- **kuku / netns 升级为广播催办**：两者最后报告分别为 `02:54:19Z` / `02:13:52Z`（均为 `6f07562`），静默均超 6 小时，且上一轮针对 `8c84e73` 的具体催办**无任何回应**——连续两轮无响应，触发广播。已发四路公告（Hub 四路 ok:true），各自指定唯一任务项：
  - kuku：① relay SIGTERM 必须 exit 0（非 143/1006）② quick 末尾必须 `通过 N, 失败 0` rc=0，且故意制造失败时 rc=1 ③ serve-discovery SIGTERM 优雅退出。
  - netns：① `407fde3` NAT 方向性复测（正向必须通、反向结论必须明确写出）② RSS 基线 20 点 min/max/均值与 15MB 目标对比 ③ 50MB bench 后回落采样。
  - 公告同时声明"缺一项就写缺哪一项，不要整份不报"，避免空报。

**本轮本地开发（全部留在 worktree，未推送）**
1. **P-3 bench frame 复用**（`internal/mesh/bench.go`）：收发两侧改用复用帧缓冲。32MiB 双向累计分配 **699.34MB → 74.08MB（−89.4%）**；`readBenchFrame`(288MB/41%) 与 `sendBenchData`(277MB/39%) 退出分配榜，剩余全在 quic-go/socket 内核层。**吞吐未变**——回环 1MiB 103–113、8MiB 86–101、32MiB 71–104 MiB/s，P-3 ≥200 MiB/s **仍未达标**，瓶颈不在分配而在传输路径。新增 `bench_frame_test.go`（帧字节等价、复用 buffer 无残留字节、坏长度拒绝）与 `bench_perf_test.go`（可重复口径）。
2. **K-1 自动化覆盖**（`internal/cli/relay_stop_test.go`）：`cmesh relay` 优雅停止此前**零进程级测试**。`internal/cli/relay.go` 抽出 `serveRelay` 供测试驱动。**红→绿已证**：移除 `srv.Shutdown` 调用 → 测试红（"relay did not stop within 20s of the stop signal"）；恢复 → 绿。
3. **F-1 接入 CI**（`.github/workflows/ci.yml`）：`packaging/install_test.sh` **从未**进入 CI——这正是 F-1 连续逃逸两次的根因（"CI 绿"对安装器毫无意义）。已加入，本地 rc=0。
4. **文档偏差修正**（`docs/architecture/MESH-DESIGN.md`）：§1.2"新建文件"表把 `internal/mesh/route.go`、`relay.go`、`agent.go`、`internal/e2e/e2e.go` 列为文件——**四者均不存在**（已逐一 `ls` 核对）。已加状态列并写明"当前 mesh 只有直连 QUIC，relay-only 报错 rc=1 不伪造"。
5. **仓库卫生**（`.gitignore`）：补 `*.test` / `*.prof` / `*.out`。本轮 profiling 产出的 12MB `mesh.test` 二进制此前可被误提交，已清理。
6. **测试 flaky 缓解**（`internal/mesh/direct_test.go`）：全量 `-race` 下出现一次 `TestPingRoundTrip` 的 QUIC 拨号超时（`context deadline exceeded`）。定位为 runner 负载下握手超过测试自设 5s 截止，非产品缺陷；改为与生产一致的 10s，随后 `internal/mesh` 连跑 6 次 `-race` 全绿、全量 `-race` 再跑 3 次全绿。

**四线研究代理（并行派发，全部只读、无 commit/push/merge）**
- **协议审计**（`/tmp/relay-protocol-audit-round49.md`）：确认 `mesh ping/bench/msg/send-to/run` **100% 只走 direct QUIC**，relay-only 报错 rc=1 不伪造；stage-1 与 mesh relay **无代码复用**；`--p2p` 是 stage-1 的 relay→direct 升级而非 direct→relay 回退。发现四类关键件全缺：路径选择接口、错误分类（无 sentinel error）、device-ID 路由表、E2E 层（走 relay 则 B 看到明文）。共 19 条文档-代码不一致（D-1..D-19）。
- **架构测绘**（`/tmp/arch-map-round49.md`）：19 个 Go 包，依赖图 DAG 仅两个零依赖基座（`internal/identity` 扇入 18、`internal/wire` 扇入 11）；**两条业务链完全不相交**，仅共享 `internal/identity`。CLI 命令注册表在 `internal/cli/cli.go:41-93`。发现 5 个缺陷：`--p2p-legacy` 死开关（解析后从未传给 `NodeConfig`，`internal/p2p` 约 1100 行不可达）、`internal/p2p2` 4710 生产行零生产导入、`KindExec` 白名单无实现、relay SIGTERM 无进程级测试（本轮已补）、`install_test.sh` 未接 CI（本轮已补）。
- **M4 门禁审计**（`/tmp/m4-audit-round49.md`）：**M4 未收口**，四个门禁中①代码+本地门禁绿 ✅ ②修复进 develop ✅ ③同 SHA 外部复测 ⚠️部分 ④发布资产 ⚠️部分。两个外部条件确认仍缺：reboot 恢复（仓库内**无任何 reboot 脚本**，已写出最小可执行清单 steps 0–5）与 darwin/windows 归档（`gh release view` 实测 **10 个资产**，矩阵需 20，`windows/arm64` 完全不存在且不在 SHA256SUMS 中）。另发现 Windows 命名三处不一致（release 无 `.exe` / installer 要 `.exe` / mkrelease 出 `.exe`）、`releases/latest/download/...` 404（无正式 release）、**已发布的 rc1 二进制不含 K-1 修复**（`strings` 比对）、发布资产来源不可追溯。
- **性能研究代理**：本轮仍在运行，报告未产出（`/tmp/perf-agent-round49.md` 尚不存在）。已知结论以本会话实测为准。

**门禁（推送前，全绿）**
- `gofmt -l internal/ cmd/ tools/` 无输出；`go vet ./...` PASS；`go build ./...` PASS
- `go test ./... -race -count=1` PASS（18 包）
- `bash tests/run-tests.sh --quick` → `通过 43, 失败 0`，rc=0
- `sh packaging/install_test.sh` → rc=0（"packaging install tests: ok"）
- 交叉编译 PASS：linux/amd64、linux/arm64、windows/amd64、darwin/amd64、darwin/arm64

**M4 现状**：未收口。剩余外部条件三项——① kuku/netns 对同一 SHA 的复核（已广播催办，未回）② darwin/windows 归档资产重建上传（实测缺 10 个，`windows/arm64` 全缺）③ reboot 恢复验收（需网主授权重启生产服务器或提供可重启 disposable VM）。

### Round 49 续 · 性能代理回报 + F-1 新缺陷修复（2026-11-16 · 值守）

**性能研究代理完整报告已产出**：`/tmp/perf-agent-round49.md`（45 KB），A/B 证据在 `/tmp/cmperf/`。代理全程只读，未改仓库、未 commit/push/merge。关键发现：

| # | 发现 | 证据 | 边界 |
|---|---|---|---|
| **F1** | **discovery 成员流热推送缺陷**：任意一次性命令后，被用成员的 anchor 推送速率从 **158 B/15s → 15.8 MB/15s（1.05 MB/s）且无界** | 已复现、已定位到行 | Docker bridge（真实进程/WS/证书钉扎） |
| **F2** | **direct QUIC 在真实 WAN 条件下吞吐崩塌**：0.1% 丢包 + 20ms 延迟 → 16.9 → 5.6 MiB/s（−67%）；0.5% 丢包 → **1.2 MiB/s（−93%）**。机制是 quic-go 用 **Cubic**（`internal/ackhandler/sent_packet_handler.go:134`），`beta=0.7` 每次丢包砍 cwnd 30% | netns + netem，3 次重复 | **模拟 WAN**，非公网 |
| **F3** | bench frame 复用结论**独立复现**，幅度更大：342.9MB → 21.8MB（−93.6%），峰值 RSS 29,133kB → 24,980kB（−14.3%） | 同机同 session A/B，交替顺序 | 回环上界 |
| **F4** | **回答"分配降了吞吐为什么不动"**：分配本身只占采样的 **1.4%**（`makeslice` 0.11s）；真正瓶颈是 `Syscall6` **18.18% flat**、`gcmAesDec/Enc` 9.48%、`memmove` 8.83%、`futex` 4.74%，`readBenchFrame` cum 11.99%（`io.ReadFull` 占 87.9% = 内核→用户拷贝）。**可优化的项目代码只占 profile 的小头** | pprof 火焰图（256MiB×4，2.29s，331% CPU） | 回环上界 |
| **F5** | **stage-1 relay 是当前最重的组件**：64MiB 端到端 7.93–11.38 MiB/s，relay CPU **27.0%–47.0% 单核**，峰值 RSS 16.0–17.3MB，tx 放大 1.002× | 真实 relay + SPAKE2 + 64MiB 传输，3 次 | Docker bridge |
| **F6** | **P-3 在任何条件下都未稳定达标**：回环 136.8–297.0 MiB/s（均值 227.7，**sd ≈ 80**）；netns WAN 下 1.06–1.15 MiB/s | 见报告数字表 | 回环上界 + 模拟 WAN |
| **F7** | `p2pquic.readLoop` 的 `make([]byte,n)` **确认无法直接池化**：`wire.DecodeFrameBytes` 把 `Payload` 别名到解密 buffer，`peer.byteQueue.Put` 再拷贝，别名链上还有 `p.sec.OpenFrame(msg)` 持有原 slice | 代码追踪 + 分配实测 3.01 MiB/消息 | 回环 |

**F-1 缺陷修复（本地完成，红→绿已证）**

- **根因**：`internal/discovery/node.go:199` 的 `Subscribe` 用 `close(old)` 废弃旧订阅。**关闭的 channel 永远可读**——旧订阅者的 `case <-ch:` 立即且永久触发，于是 `internal/relay/mesh.go` 的 `streamMeshMembers` 陷入"推整份快照→立即再推"的热循环，只影响被取代的那条旧流。
- **修复位置**：选在**消费侧**而非改 `Subscribe` 语义——`case _, ok := <-ch`，`!ok` 即退出（旧流已被新流取代，应静默退出）。`Subscribe` 的"关闭旧 channel"契约不变，`TestSubscribeSecondClosesFirst` 仍然成立。
- **红→绿**：新增 `internal/relay/mesh_reconnect_test.go`。
  - 修复前**红**：`orphaned stream still receiving (757 bytes): the old subscription channel was closed instead of being signalled, so it spins`
  - 修复后**绿**：旧流在 500ms 内读不到任何字节；活跃流正常收到推送。
  - 复核时手动把 `if !ok { return }` 改成空实现重跑，**再次确认红**（757 bytes），恢复后绿——红绿链完整。
- **同类缺陷排查**：全仓 `close(` 扫描，`node.go:199` 是**唯一**的"关闭订阅 channel"用法；其余 `close(done/stop/gone)` 均为"关闭即终止"语义，消费者 `case <-done:` 天然只触发一次，不构成热循环。
- **外部独立验证（性能代理自己的 lab，非本会话自测）**：代理用独立 Docker lab 复现缺陷，并对**落地后的 HEAD** 做了 A/B 对照：
  - base（缺陷版）：一次性命令后 **16,144,158 B / 12s = 1,343,108 B/s**（相对干净态 122.6 B/s 放大 **×10,957**）
  - fix（落地版 `a4080e8`）：一次性命令后 **1,816 B / 12s = 151.1 B/s**，4 次后 226.8 B/s **不累积**
  - **−99.99%**，缺陷闭合。
- **门禁**：`gofmt` / `go vet` / `go build` / `go test ./... -race -count=1`（18 包）/ `run-tests.sh --quick`（43/0 rc=0）/ `packaging/install_test.sh` rc=0 / `go test ./internal/relay -race -count=3` 全绿。

**本轮新增结论对 KPI 的影响**：P-3 的瓶颈已被定位到**传输路径级**（quic-go syscall + 加解密 + 拷贝），而非分配；后续优化方向应从"压分配"转向"减少 syscall/拷贝 + 换拥塞控制算法"，否则分配优化已无空间。

### Round 49 续二 · oh-no 字节完整性专项 + 一条上报的否定（2026-11-16 · 值守）

**oh-no-agent（`12:05:24Z`）—— `7a66f5e` bench frame 复用字节完整性专项 PASS**
- 单测 `TestBenchFrame*` `-race × count=3` 全绿（RoundTrip / NoStaleBytes / RejectsBadLength）。
- **端到端双向字节完整性**：真实 CLI + 常驻成员进程（接收侧走的就是复用 buffer 的生产路径）。m1→m2 的 4096 / 1MiB / 8MiB / 32MiB 与 m2→m1 的 32MiB，**sent 与 received 全部精确相等**。
- **对抗形态帧边界**：邻近尺寸 4095 / 4097 / 65535 / 65537 / 1048577 / 33554431 全部 rc=0、精确相等——非对齐尺寸下帧边界无损（这是 no-stale-bytes 最容易暴露的形态）。
- **跨帧残留**：同一常驻接收进程连跑 3 轮 32MiB，三轮全部精确相等，无 corrupt；交替大小轮换（1MiB+1 / 64KiB−1 / 32MiB−1）也全部精确相等。
- 面板三点顺带抽查无回退（无令牌 401 / 正确头 200 / `?token=` 401 / `/panel` → 308 → `/panel/`）。

**一条被否定的上报（不计入缺陷）**
- oh-no 报「`account create` 把 `account.json` 落在 `<dir>/account/` 子目录，而 `account device-add` / `cred create` 找 `<dir>/account.json`，首次全新部署需手工上移一层」。
- **本会话实测不成立**：`account create` 写入的就是 `<state>/account.json`；`init → account create → account device-add` 全流程平铺布局、`rc=0`，`find` 输出只有 `device.json` / `account.json` / `<id>.cert.json` 三个平级文件。
- **成因**：对方把不同命令的 `--state` 指向了不同目录（`.../account` vs 其父目录），于是看到 `account/account.json` 与 `account.json` 的错位——属**用法错误**，不是产品缺陷。已在 KPI 的外部等待一节写明"该上报已被本会话实测否定"。

**性能代理最终补充数字（对已提交结论的修正）**
- bench frame 复用的**峰值 RSS 收益是 −14.3%（29,133 → 24,980 kB）**，不是此前记录的 −3.5%——原结论偏保守，已在 KPI 更正。
- 分配降幅实测 **−93.6%（342.9 → 21.8 MB）**，与主会话的 −89.4%（699.34 → 74.08 MB）口径不同（两套 harness 计数范围不同），**比例可比、绝对值不可互换**。
- **加宽 QUIC 流控窗口经 A/B 排除、无收益**——所以 Cubic 的崩塌行为是拥塞控制本身，改进可能需换算法（BBR），不是配置疏漏。
- **16 个丢包 ≈ 吞吐掉 15 倍**。

**已推送**：`7a66f5e`（4 笔）→ `d206c14`（F-1 修复 + 性能文档）→ `47cff18`（独立验证记录）。三次推送的远端 CI 均 **success**，其中 `d206c14` 那一轮已包含新加入 CI 的「离线安装回归」步骤且通过。

### Round 50（2026-11-16 · 值守：doubao 独立实测 F-1 + 发布资产逐 URL 实测 + README 404 修复）

**Hub 处置**
- 水位推进：`12:05:24Z` → **`12:36:22Z`**。水位后 1 条 agent 报告。
- **doubao-cloud-vm-01 ✅ 独立实测 F-1 修复生效**（`d206c14`）：真实 `serve-discovery` + 常驻 `join` + 一次性命令拓扑，eth0 rx 字节差分（`/proc/net/dev`）：

| 阶段 | 窗口 | rx 速率 |
|---|---|---|
| 稳态基线（无命令） | 10s | ~1,317 B/s |
| 20 × `mesh ping` 后 | 15s | **107 B/s** |
| 持续观测 | 30s | **514 B/s** |
| 5 × `run @mem` 后 | 15s | **340 B/s** |
| 5 × `msg --text` 后 | 15s | **330 B/s** |

  30 次一次性命令后稳定在 **0.1–0.5 KB/s**，缺陷态预期 **1.05 MB/s（无界）**——相差约 **3 个数量级以上**。
- **doubao 还独立复验了红→绿链**：在 /tmp 副本把消费侧还原成 `case <-ch:` 并移除 `if !ok { return }` → `TestMeshMembersReconnectDoesNotHotPush` **红**（`orphaned stream still receiving (769 bytes)`）；未改则绿。
- **F-1 至此三方独立验证闭环**：本会话（红→绿 757 bytes）→ 性能代理自建 lab（1,343,108 → 151.1 B/s，−99.99%）→ doubao（30 次命令后 0.1–0.5 KB/s + 独立红→绿 769 bytes）。
- doubao 同时确认 `packaging/install_test.sh` 在 linux/amd64 独立复跑 **rc=0**，且 CI 第 59 行接线存在。install 线全绿。

**四线静默时长与催办升级**
| agent | 最后报告 | 静默 | 处置 |
|---|---|---|---|
| oh-no-agent | 12:05:24Z | 0.8h | fresh，不催 |
| doubao-cloud-vm-01 | 12:36:22Z | 0.2h | fresh，不催 |
| kuku-sandbox-01 | **02:54:19Z** | **9.9h** | 超 6h + 连续三轮无回应 → **全队广播** |
| netns-probe | **02:13:52Z** | **10.6h** | 超 6h + 连续三轮无回应 → **全队广播** |

广播公告已发频道 + 四个收件箱（四路 `ok:true`），并按规则**提示网主唤醒**。公告中给 kuku/netns 各指定了**唯一的三项任务**，并明确写「若已离线，请在 Hub 留一句话说明原因，我据此改派，不占其他环境额度」。

**本轮新查实的发布资产缺陷（逐 URL 实测，非只看 gh 列表）**

`gh release view v1.0.0-rc1` → tag `v1.0.0-rc1`、`prerelease=true`、**资产 10 个**；`git tag -l` 只有这一个 tag，**没有非 prerelease 的正式 release**。

| 文件 | 状态 |
|---|---|
| `cmesh-v1.0.0-rc1-linux-amd64` / `-linux-arm64` / `-darwin-arm64` | **206 可用** |
| `cmesh-v1.0.0-rc1-darwin-amd64` | **404** |
| `cmesh-v1.0.0-rc1-windows-amd64`（无 `.exe`）与 `-windows-amd64.exe` | **404（两种命名都不存在）** |
| `cmesh-latest-linux-arm64` / `-darwin-amd64` / `-darwin-arm64` / `-windows-amd64(.exe)` | **404** |
| `releases/latest/download/cmesh-latest-linux-amd64` | **404** |
| `releases/download/v1.0.0-rc1/cmesh-v1.0.0-rc1-linux-amd64` | **200（16,938,340 B）** |

- **SHA256SUMS 列 15 行，实际资产 10 个 → 7 行悬空**（列了 `cmesh-latest-darwin-amd64`、`cmesh-latest-linux-arm64`、`cmesh-latest-windows-amd64`、`cmesh-v1.0.0-rc1-darwin-amd64`、`cmesh-v1.0.0-rc1-windows-amd64` 等，但这些文件从未上传）。`sha256sum -c --ignore-missing` **恰好跳过它们** → 「校验通过」是假象。
- **`windows/arm64` 连 SHA256SUMS 都没有**。
- **命名约定以 `.exe` 为准**：`tools/mkrelease/main.go:283-288`（`latestAlias` 对 `.exe` 优先）与 `packaging/install.sh:90`（`suffix=.exe`）都按 `.exe` 工作 → **是这次发布没照约定上传**，不是代码不一致。
- **已发布的 rc1 二进制不含 K-1 修复**：`strings | grep -c "relay: shutdown"` = release **0** / HEAD **1** → 此前"SIGTERM 优雅停止 PASS"的 16/17 结论来自 K-1 之前的资产，**不覆盖当前代码**。
- **已发布的 rc1 二进制版本串是 `cmesh dev`**（发布时未带 `-ldflags "-X ...cli.Version="`）→ 运维无法从二进制知道它是哪个 commit 构建的，**资产来源不可追溯**。
- **安装器实测**：`CMESH_VERSION=v1.0.0-rc1` → **rc=0**，`versions/v1.0.0-rc1/` + `current` symlink + `.cmesh-install-root` 齐全，`--uninstall` rc=0；默认 `VERSION=latest` → **rc=1（404）**。

**README 404 缺陷（本地已修，待放行）**
- README §1 原本让用户从 `releases/latest/download/cmesh-latest-linux-amd64` 下载。GitHub 只对**非 prerelease** 的正式 release 提供该路径，仓库唯一 release 是 prerelease → **实测 404**，每个照做的访客都拿不到东西。
- 已改为版本化地址 `releases/download/v1.0.0-rc1/cmesh-v1.0.0-rc1-<os>-<arch>`，并**端到端实测**：下载 rc=0 → `sha256sum -c SHA256SUMS --ignore-missing` 输出 `cmesh-v1.0.0-rc1-linux-amd64: OK` → `install` 落盘成功。
- 同时补明：darwin/amd64 与 windows 资产**尚未上传**、Windows 带 `.exe` 后缀、`--ignore-missing` 会掩盖「清单承诺但文件不存在」（请确认自己那行显示 `OK`）。
- 提交：`1712099` docs(readme)。

**本轮派发的四条研究/验证线（全部只读或实验，均未 commit/push/merge）**
1. **性能：传输路径级优化实验** —— 目标：补齐每流重传计数（`Retransmissions` 现为 nil，是所有拥塞控制改进的度量前提）、量化「减 syscall/拷贝」的**本仓可控空间**、评估 Cubic→BBR 可行性、测 relay 7.93–11.38 MiB/s 是否本仓可优化。
2. **M4 发布资产与安装闭环审计** —— 目标：资产矩阵机器可判定、Windows 命名最小修复方案、systemd unit 模板交付物、资产可追溯性、网主一次性动作清单。
3. **mesh relay trunk 协议设计** —— 目标：裁定 R-3.2「网内节点中转 vs 专用 relay」的口径冲突，产出可施工的 vertical slice 规格（帧、一跳 device-ID 认证、A↔C E2E、route open、direct-first fallback、quota）、前置件清单、可执行验收清单（含**反向用例**：认证失败不得回退）、v1.1 边界、风险表。
4. **授权云服务器真实链路复测** —— 目标：在 175.24.139.148 上用当前 HEAD 起临时 anchor，从本机经**真实公网**实测 status/ping/msg/send-to/run/bench 与反向可达性、F-1 公网形态复核、资源指标（RSS 20 点 vs 15MB 目标）、rc1 真资产安装闭环；不改 nginx/hub、不重启、测完清理。

**本轮门禁**：README 改动为纯文档，不涉及代码路径；改动前 `gofmt`/`vet`/`build`/`go test ./... -race`（18 包）/`run-tests.sh --quick`（43/0 rc=0）/`packaging/install_test.sh` rc=0/交叉编译 5 平台**均已绿**（承上一轮 `3ca269e` 的门禁结果，本轮代码零变更）。

### Round 50 续 · 三线回报 + 端点 fallback 缺陷修复 + 资源 KPI 口径厘清（2026-11-16 · 值守）

**三线回报（均 PASS，F-1 四方独立验证闭环）**

1. **netns-probe（第五十五轮，`3ca269e`）** —— 终于回报（静默 10.6h 后）。NAT 方向性：入网 **0.565 s**，`mesh ping --count 30` **30/30、loss 0、p50 0.395ms、p95 0.537ms、link=direct**；反向锚点→客户端**仍不可达**（`sendmsg: network is unreachable`），**第四次同形**，R-3.2 边界、不报缺陷。
2. **doubao-cloud-vm-01（`3ca269e`）** —— Windows 资产命名专项。给出**三层失败链**：① `latest/download` 别名对 rc1 整体不可用（`curl -sSI .../releases/latest` → 302 到 `.../releases`，**latest 不指向 rc1**）→ 默认 `VERSION=latest` 下**任何平台都在 checksum 步骤 rc=1 失败**；② rc1 的 expanded_assets 实测 10 条、**0 个 windows 条目**（逐 URL 404），而 SHA256SUMS 却登记了 windows 行 → **登记行在、文件没传**；③ `.exe` 命名不一致坐实（install.sh:90 请求 `.exe`、mkrelease:283 出 `.exe`、SHA256SUMS 登记无 `.exe`）——即便未来按无 `.exe` 补传，install.sh:133 的 `awk '$2==name'` 也找不到 `.exe` 行 → `the release does not list a valid checksum`。**与本会话独立实测结论一致。**
3. **oh-no-agent（`3ca269e`）** —— bench 字节完整性 + F-1 外部 A/B。字节完整性：**双端双向** 4096/1MiB/8MiB/32MiB 全部 `sent == received` 精确相等；对抗尺寸 4095/4097/65535/65537/1048577/33554431 全无损；同进程连跑 3 轮 32MiB 无跨帧残留。F-1 A/B（`ss -tinp` 的 `bytes_sent` 累计采样）：

| 二进制 | 8 次一次性命令后的推送增量 | 后续 15 s 稳态 |
|---|---|---|
| `7a66f5e`（缺陷版） | 累计 **645,532 B** | 每 15 s **+1.37MB / +1.58MB / +1.81MB（≈100KB/s 且不收敛）** |
| `3ca269e`（修复版） | **12 B** | 稳定 KB 级，不随命令次数累积 |

**F-1 至此四方独立验证闭环**：本会话（红→绿 757 bytes）→ 性能代理 lab（1,343,108 → 151.1 B/s，−99.99%）→ doubao（30 次命令后 0.1–0.5 KB/s，独立红→绿 769 bytes）→ oh-no（缺陷版 645,532 B 且不收敛 vs 修复版 12 B）。

**★ 新缺陷：多端点 fallback 整体失效（mesh 中继干道设计代理发现，本会话修复）**

- `internal/mesh/direct.go` 的 `DialDirect` 在端点循环里**复用同一个 UDP socket**，而 `dialOne` 在失败时 `udp.Close()`。quic-go 的 `quic.Dial` 把 `net.PacketConn` 包进 **single-use Transport**（`quic-go@v0.55.0 transport.go: isSingleUse`），会话结束即关连接 → **第一次尝试失败后，后续端点全在已关闭的 socket 上拨号**，报 `use of closed network connection`。
- **后果**：成员宣布多个端点（私网 + NAT 映射）时，**fallback 永远走不到活端点**——这也是「直连失败 → 回退中继」的前置阻塞（不修它，v1.1 的回退路径在多端点场景根本走不到）。
- **红→绿**：新增 `internal/mesh/direct_fallback_test.go`（第一个端点是无应答端口、第二个是活 listener，断言链路在第二个端点建立并能 ping）。修复前红：`all 2 endpoint(s) for ... failed: use of closed network connection`；修复后绿。
- **修法**：每次尝试单独 bind 一个 socket（quic-go 的 single-use 语义决定了一个 socket 只能服务一次拨号）。
- 提交：`ca4f338` fix(mesh)。

**★ 资源 KPI 口径厘清（本会话本地实测，化解 5 倍读数差）**

netns 报「RSS 增长消失（20 点全平 75,268 KB），但稳态仍是 15 MB 目标的 5 倍」；历史上 KPI 记的是「post-GC 相位 16,360 KB」。本会话在 macOS 上用 `ps -o rss` 采 150 秒窗口：

```
t=5s   RSS=25,968 KB
t=10s  RSS=25,952 KB
t=15s  RSS=7,712 KB     ← 运行时的 scavenger 把空闲内存还给了 OS
t=20s  RSS=7,888 KB
...    稳定 7.7–9.7 KB 区间，末值 9,744 KB
```

`GODEBUG=gctrace=1` 佐证：`gc 1 @0.002s (64->64->64 MB)`、`gc 2 @120.282s (65->65->0 MB)`。

- **结论**：稳态 **7.7–9.7 MB，低于 15 MB 目标 → 达标**。前 15 秒的 25–26 MB 是**启动 churn + 运行时尚未回收**，不是稳态。
- **netns 报的 75,268 KB 全平是「pre-GC 相位」**——与上轮的 16,360 KB 是**两个不同相位**，不可直接比。两份报告都诚实标注了各自口径，只是没对齐。
- **资源 KPI 从「未达标」改记「达标（post-GC 稳态口径）」**，并写明采样窗口要求：前 15 秒内的读数会误判为超标。
- bench 峰值 RSS（历史上 ~139 MB）本轮未复现（netns 测得 75,268 KB 全平、零增长），与 bench frame 复用方向一致；**但 netns 明确声明不做因果归因，本会话亦不归因**。

**本轮派发的四条线**
1. **mesh relay trunk 协议设计**（已完成）：裁定「中转方 = 网内成员节点」（R-3.2「不是专用中继」胜出；决定性依据是**锚点必须本身是成员**，所以现网里专用 relay 就是成员进程内的一个角色）；裁定为成员即中转方后，**E2E 加密从可选变成必须**（中转方在信任模型内，密码学上必须封住它），需 SIGMA（`golang.org/x/crypto` 已在 go.mod，无新依赖）；quota 记在中转成员**自己账上**，必须**旁路记账**；交付 5 个新 wire 帧、3 消息握手序列、6 个 sentinel error（含互斥分类表）、`PathPicker` 接口（`Picker==nil` 时行为与今天逐字节相同）、**7 项前置件（含每项红→绿证法）**、**10 条反向用例（N1 回退成功 rc=0 vs N2 认证失败不回退 rc=1，附 direct 拨号次数断言）**、14 项风险。**另有两个实测发现**：① 上述 fallback 缺陷（已修）；②「远端拒绝我方证书」时 `Dial` 返回 **nil**，错误在 ack 读写阶段才浮现为 `CRYPTO_ERROR (remote)` → fallback 判定必须覆盖**整个会话建立过程**，不能只看 `DialDirect` 返回值，否则「认证失败不回退」这条需求根本测不出来。
2. **性能：传输路径级优化实验**（运行中）
3. **M4 发布资产与安装闭环审计**（运行中）
4. **授权云服务器真实链路复测**（运行中）

**本轮推送**：`1712099` docs(readme)（README 安装命令 404 修复）→ `ca4f338` fix(mesh)（端点 fallback）。两次推送前门禁全绿（fmt/vet/build/race 18 包/quick 43-0/install 回归/交叉编译 5 平台）。

### Round 51（2026-11-16 · 值守：kuku 回归 + 安全缺口修复 + relay 性能缺陷）

**Hub 处置**
- 水位推进：`12:36:22Z` → **`14:31:10Z`**。水位后 5 条 agent 报告（netns、doubao、oh-no、kuku、oh-no 二次）。
- **kuku 终于回报**（静默 9.9h 后），三项指定任务**全部按判据通过**：
  - **relay SIGTERM**：三轮 exit **0**（43–46ms），端口 **0.0–0.1ms 内可再 bind**（判据 6s）。
  - **serve-discovery SIGTERM**：五轮 exit **0**（28–34ms，另有一次 6.05s 长尾未复现，如实标注不判缺陷）。
  - **e2e 退出码契约**：绿 → rc=0；刻意失败 → rc=1 且计数与失败项原文一致。
  - 如实申报：**e2e 原样执行 rc=1（`通过 42, 失败 1`）**，失败项是 `mkfifo` 被沙箱 seccomp 拦截（EPERM），代理用进程替换适配后得 43/0 → 判为**环境限制**、非产品缺陷。
  - loopback 三进程基础连通 PASS（ping 10/10 link=direct、msg 40B 逐字节一致、run @info rc=0）。
  - **6 条观察项**，其中一条是**安全观察**（见下）。

**★ 安全缺口：出口地址策略只实现了一半（kuku 发现，本会话修复）**

- **根因**：`internal/discovery/member.go:179` 的 `allowedDiscoveryHost` 只判断**字面 IP**，其注释把剩下的交给拨号方——「callers that dial them must use an egress policy that revalidates resolved addresses before connecting」——**但从未实现**：`internal/wsconn/wsconn.go:102` 用的是裸 `net.Dialer`，**解析后的地址无人复核**。
- **后果**：发现地址是**运维提供的值** → 一个解析到回环/私网的主机名可直连，是 **SSRF 形态**。kuku 在沙箱实测 `disc.local → 127.0.0.1` **穿透成功**（用 `/etc/hosts` 绕行回环 disc 限制时发现）。
- **修复**：
  - `wsconn.DialOptions` 增加可选 `EgressPolicy func(net.Addr) error`，经 `net.Dialer.Control` 在 **DNS 解析后、socket 连接前**回调——这是唯一能拿到「名字实际指向哪」的时机。
  - `internal/discovery/egress.go` 提供 `EgressPolicy`：解析后地址若为 loopback/私有/link-local/未指定/保留/CGNAT/文档段 → 拒绝。
  - discovery 客户端**始终**传入该策略（新增 `ClientConfig.EgressPolicy` 与 `AllowLocalDiscovery` 两个开关，后者**仅供本地测试 harness**，生产从不设置）。
  - **stage-1 relay 保持原行为**：其端点本就常是运维自己网络上的私网地址（有意不启用）。
  - **e2e 套件的逃生口用 build tag 隔离**：`internal/cmeshtest` 带 `//go:build cmeshtest`，只有 `tests/run-tests.sh` 用 `-tags cmeshtest` 构建。**`go build`、CI 构建、发布构建都不带该 tag** → 出货二进制没有逃生口；`go tool nm` 实测确认 `internal/cmeshtest` **未链接进生产二进制**，并有测试断言之（`TestProductionBuildHasNoEscapeHatch`，带 tag 构建时该测试会红）。
- **红→绿**：去掉策略接线后 `TestClientPassesEgressPolicyToTheDialer` 红（"the client dialled without an egress policy"），恢复后绿；`internal/wsconn` 另有用真实 listener 证明「策略在连接**之前**生效」（连接根本没被接受）。
- 测试 fixture 的处理：把逻辑主机名重定向到回环的 fixture **显式** `opts.EgressPolicy = nil`，而不是让 fixture 悄悄削弱策略。

**★ relay 前向循环读缓冲未复用（性能代理发现，本会话修复）**

- `internal/relay/relay.go` 的前向循环把一个读缓冲穿过 `RecvInto`。消息放不下时 `RecvInto` 在**自己内部**扩容并返回扩容后的切片，但循环读的是**自己的** `buf = buf[:cap(buf)]`——**从未看到那次扩容** → 每条大于初始 64KiB 的消息都把 64→128→256→512 KiB 的分配+拷贝链**重付一遍**。
- 实测 relay 的真实消息**平均 493 KiB**（远超初始缓冲）→ 这是常态而非边角。profile 里 `makeslicecopy` 占 relay CPU **23.73%**；修复后 4.88%，ABAB 各 9 次测得 **relay CPU 中位 0.310s → 0.090s（−71%）**、吞吐中位 +11%、最坏 +231%，64 MiB SHA-256 校验通过。
- **红→绿**：新增 relay 侧缓冲生长计数（`readBufferGrowths`）。修复前测试红：**"grew its read buffer 20 times for 20 messages"**；修复后绿。
- **「批量写」明确不做**：64 MiB 只走 136 条 WS 消息（493 KiB/条）已摊薄；且**协议禁止**（1 WS 消息 = 1 AEAD 帧，`internal/secure/secure.go:403`）——本仓 `internal/peer/pump.go:280-290` 注释记录了**同一原因已删过一次 batching**。

**★ 性能代理本轮的两处「推翻上一轮结论」**

1. **「quic-go 不暴露重传计数」是错的**：它暴露的是**每连接**计数——`Conn.ConnectionStats().PacketsLost`（`quic-go/connection.go:800-813`）+ `logging.ConnectionTracer.LostPacket`（`logging/connection_tracer.go:28`）。已在 /tmp 副本实现 `BenchStats.Retransmissions` 从 nil 变真实值 + `BenchLoss`/`PeerLoss` 双向上报，两条独立计数器实测一致（98/98、369/369、175/175、207/208，差异为 spurious）。**上一轮的「度量前提阻塞」已打开**。
2. **拥塞控制是 Reno 不是 Cubic**：`internal/ackhandler/sent_packet_handler.go:134-140` 明写 `true, // use Reno`（上游 v0.55.0 与 v0.65.0 双向核对）。Reno 的 `renoBeta=0.7` 与 Cubic 的 `beta=0.7` **数值巧合**，所以「砍 30%」数字碰巧对，但**算法名错、「换 Cubic」路线根本不存在**。**换拥塞控制不可行**：quic.Config 无 congestion 字段，编译报 `use of internal package .../internal/congestion not allowed`，上游最新 v0.65.0 仍硬编码 Reno → **唯一路径是 fork quic-go**。
3. **P-3 崩塌机制直接测量**：cwnd 追踪显示 20ms/0% 丢包时 `cwnd_max`=5.6 MB，1% 丢包掉到 111–186 KB（**−30~50×**），吞吐 39.6→1.7 MiB/s；同档 111 个丢包含 **40 个 spurious**，cwnd 反而保住 1.98 MB → **证明是 cwnd 塌缩而非丢包数决定吞吐**。
4. **「减少 syscall」本仓可控空间 ≈ 0**：`Syscall6` 23.59% flat，逐层 `-peek` 全部在 quic-go 内部；本仓仅 9.30% cum。**darwin vs Linux 差 2.3–3.4×，根因是 GSO 仅 Linux** —— Linux **604–638 MiB/s（P-3 达标）**，darwin 233–270（不达标）。

**M4 审计代理的两处「更正上一轮结论」（本会话已独立核实）**
- **「仓库内无 systemd unit」不成立**：`packaging/systemd/` **存在**（`cmesh-discovery.service` + env 示例 + README）。但有 5 处实质缺口，已修（见下）。
- **「发布资产来源不可追溯」不成立**：`go version -m` 显示已发布二进制**内嵌 VCS 信息**（`vcs.revision=6f07562d37e31358b7ff18dbeecb1f02936f8dc1`、`vcs.time=2026-11-15T19:53:33Z`、`vcs.modified=true`），审计代理用「干净 clone + `-trimpath` + 复现 `+dirty`」配方**字节级复现 5/5 digest 精确命中**。真正的问题是**仓库里没把这个写下来**。
- 另更正：**已发布 rc1 含 serve-discovery 的 K-1，不含 `relay` 的 K-1**（此前用 `strings` 查 `relay: shutdown` 命中 0 是因为查错了路径）。

**systemd unit 五处缺口（本地已修，`79c6891`）**
① `ExecStart` **无 `--account-pub`** → 缺该参数时发现节点**根本不挂载入网端点**，凭证入网不可用；② 无 `StateDirectory=`（`ReadWritePaths` 指向 fresh 机器上不存在的目录）；③ 无 `TimeoutStopSec=`（与 relay 10s drain 不对齐）；④ `EnvironmentFile=-` 的 `-` 前缀让文件缺失不报错 → 变量全空 → `--state ""` 静默回落 `~/.cmesh`（且与 `ProtectHome=read-only` 冲突）；⑤ 空口令在无 tty 时 EOF。已加 `StateDirectory`/`StateDirectoryMode`、去掉 `-` 前缀、补 `--account-pub`、显式 `KillSignal=SIGTERM` + `TimeoutStopSec=15s`，env 示例补注「账号公钥不是秘密」。

**本轮推送**：`1712099` docs(readme) → `ca4f338` fix(mesh) 端点 fallback → `79c6891` fix(systemd) → `aaf42ab` fix(relay) 读缓冲复用 → `75a8ff7` fix(discovery) 出口策略。每次推送前门禁全绿（fmt/vet/build/race 18 包/quick 43-0/install 回归/交叉编译 5 平台）。

### 工作模式固化（2026-11-16 网主定调后写入）

网主原话：**「就是你一个人开发肯定慢的，你是作为主代理去拍的，代理去开发研究，然后你去收集数据反馈和指导方向，懂吧？」**

本轮数据直接印证了这条，因此写进工作规则（每轮值守照此执行）：

**分工**
- **主代理（值守）**：定线路、并行派发、收数据、**独立复核**、纠偏、定方向、把结论写进三处正典（DEV-KPI / ROUND-LOG / 看板 HTML）。**不埋头写实现。**
- **子代理**：只读研究（架构测绘 / 协议审计 / 性能 profiling）、只读设计（中继干道规格）、限文件范围可改（/tmp 副本 A/B、候选修复验证）、外部真实环境复测（Hub 四家）。**全部禁止 commit/push/merge。**

**本轮铁证——主代理自己的结论被推翻 4 次**（这就是"拍"与"开发"必须分开的原因）：

| 主代理的结论 | 被谁推翻 | 正确事实 |
|---|---|---|
| 「quic-go 不暴露重传计数，度量前提被阻塞」 | 性能代理 | 暴露的是**每连接**计数（`ConnectionStats().PacketsLost` + `ConnectionTracer.LostPacket`），阻塞不存在 |
| 「拥塞控制是 Cubic，换 BBR 可行」 | 性能代理 | 是 **Reno**（`renoBeta=0.7` 与 Cubic `beta=0.7` **数值巧合**，故数字对名错）；换算法**不可行**，需 fork quic-go |
| 「仓库内无 systemd unit」 | M4 审计代理 + 主代理复核 | `packaging/systemd/` **存在**（只是有 5 处缺口） |
| 「发布资产来源不可追溯」 | M4 审计代理 + 主代理复核 | 二进制**内嵌 VCS 信息**，用「干净 clone + `-trimpath`」可**字节级复现 5/5 digest** |

**反过来，子代理抓到的 3 个真缺陷全是单干找不到的**：多端点 fallback 整体失效（协议审计代理）、relay 前向循环读缓冲未复用（性能代理，CPU −71%）、出口地址策略只实现一半（外部 kuku，SSRF 形态）。

**每轮硬性要求**
1. **并行派发 ≥4 条互不重复的线**：架构/依赖地图、协议/验收审计、性能 profiling、安装/发布闭环、授权云真实链路、M4 外部验收清单；**每轮至少一条性能线**。
2. **派单写清**：目标 / 文件范围（只读还是限范围可改）/ 命令 / 环境 / 样本数 / 数字 / 缺口 / 下一步；**禁止把方案写成完成**。
3. **子代理结论一律先独立复核再采信**（本轮 4 次推翻全部靠这条纪律）。抽一条独立验证：读源码、复跑命令、或另派一个代理。
4. **失败如实记**：本轮授权云复测线**两次失败**（第一次模型空转、第二次 API 网络断开），第三次重派才启动——**失败就是失败**，不能当已覆盖。
5. **推翻自己时明写"更正"**：错的是哪句、正确的是什么、谁推翻的；保留错误的原记录，别悄悄改掉。
6. **只在聊天里说不算数**：结论必须落到 DEV-KPI（数字与判定）、ROUND-LOG（过程与证据）、看板 HTML（用户每天看的入口）。
