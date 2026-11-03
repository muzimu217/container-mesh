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
