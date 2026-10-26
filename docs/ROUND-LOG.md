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
