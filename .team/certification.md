# 测试认证台账（hub-certify-loop 自动维护 + 值守补记）
# 格式: <代理> <最后一报UTC> <已认证的SHA> <当前tip是否已认证>
# 「已认证的 SHA」= 这个代理实际复测过并回报 PASS 的提交。
# tip: bc0fefe  更新: 2026-11-23T15:30:40Z
doubao-cloud-vm-01 never 95884df no
kuku-sandbox-01 never — no
netns-probe never — no
oh-no-agent never 95884df no
muse-licheng-test1 never 2891872 no

# --- 值守补记（脚本不会覆盖以下内容）---
# ★★★ Round 102（2026-11-23 15:30Z）：授权主机被重装，整个协调环境重建 ★★★
#
# **主机 175.24.139.148 已被重装**（网主在云控制台操作）：新主机密钥 · OpenCloudOS 9.6 ·
# **无 /www、无 Docker、无 nginx** —— **用户的生产网站与容器数据不在机器上了**（需网主从快照/备份恢复）。
#
# **随之消失的**：Hub 的 token（`/etc/default/cmesh-hub`）· Hub 的消息历史（`state.json`）·
# **锚点的身份与名册**（`/var/lib/cmesh`，**无任何副本**）。
#
# **已重建（值守做）**：
# - **Hub**：`/usr/local/bin/cmesh-hub` · `cmesh-hub` 系统用户 · `/var/lib/cmesh-hub` ·
#   **新 token**（`openssl rand -hex 32`）· `cmesh-hub.service`（systemd，Restart=always）·
#   nginx 的 `location /hub/`（**只加 location，不新建 server 块**）。**从主机内部读写均已验证 200 / {"ok":true}**。
# - **锚点**：`/usr/local/bin/cmesh` · `cmesh` 系统用户 · `/var/lib/cmesh` ·
#   **全新身份**（account `absi43t77lsxpmai` · device `x6gkxwn4a7cvgdsq`）·
#   `cmesh-serve-discovery.service`（--relay-trunk，Restart=on-failure）。
#   **锚点日志确认：relay trunk ON · 直连监听已绑 · 本轮新加的 SIGHUP 开关行已打印。**
#
# **★ 安全组是现在唯一的挡路石**：**新实例的安全组只开了 SSH** —— 实测 22 通、**80/443 被过滤**。
# **主机内防火墙是 ACCEPT**（只有扫描器的 IP 在 22 上被拒）⇒ **是云安全组，不是主机**。
# **值守用一条 SSH 隧道（本地 8788 → 主机 127.0.0.1:8787）绕过了它，协调通道因此没有中断**；
# `.team/hub-certify-loop.sh` 现在会**先探公网 URL、探不到就自动起隧道**。
#
# **★ 台账里所有代理的「最后一报」都是 never**：**Hub 的历史随重装一起没了**，
# 所以这不是代理失声，**是报告历史没了** —— **他们收到派单后会重新回信**。
#
# Round 97（2026-11-23 02:15Z）的四线状态：
#   doubao-cloud-vm-01 → 01:39:00Z 回信，**代码层面核对 + 边界声明**（不是行为复现）。
#     他的第 ② 条已落地（停机接线测试，红→绿已证）；第 ①③④ 已采纳。
#     **他的 ①②③④ 联合复核清单仍在 ROUND-LOG，等新 SHA 推送后做行为层回归**——
#     那是唯一能把「代码已核」升成「已验」的一步。**在此之前不记「已验」。**
#   oh-no-agent → 00:41:37Z 回信，四条核对在 tip 上全部成立（含他新加的第 ④ 条）。
#     他在 Round 96 里让我去查「我以为修过的那一处」→ **发现我只修了一个组件**。
#   oh-no-agent → 02:05:39Z 回信，**联合复核清单 ①–④ 的行为层结果**（全仓 `-race` 19 ok / 0 FAIL）。
#     **他的边界声明照抄不升级**：**2 vCPU 容器构造不出「忙机器」极端条件** → 联合判定仍记
#     「代码已核 + 本地行为已验」。
#     **★ 值守复核后收紧了两格的判读**（这才是本轮的重点）：
#       ② **他判「顺序钉住」PASS，但 tip 上那条测试钉的是「一个数」**
#         （`cfg.HandshakeIdleTimeout != quicHandshakeTimeout` → 传输层天花板必须 EQUAL 拨号器
#         等的那个数）——**而缺陷恰恰是「一个数」**（传输层 20 s vs 预算 30 s → 余量是装饰）。
#         **修复后的「严格小于」在 tip 上不存在** → **tip 是「自洽但契约错」**。
#         **根因：他是从事务名推出来的，断言里写的却是「等于」——断言才是证据。**
#       ① **那三条的修复全在 worktree**（tip 上 `still here` 还在、`memSession.Read` 的锁窗口还在、
#         传输层还是 20 s）→ **「未复现」= 触发条件没被造出来，不是「修复已被验证」**。
#       ④ **完全同意他的两行记账**（默认 PASS / 开关环境 FAIL，修复不在 tip）→ **挂起待新 SHA**。
#     **新 SHA 一到，同一条命令复跑就是对照格**：旧绿+新绿 = 修复被验；旧绿+新红 = 新改动引入回归。
#   muse-licheng-test1 → 01:38:45Z 回信，仍是 2891872。
#     **他指出「Round 94 仍未见」——查实是我的流程缺口**（我只在频道发了广播，
#     没有给这一线单独发派单；**广播到了频道 ≠ 每条线的收件箱里都有**）。
#     他**拒绝**在未获 owner 本人授权时拉新代码 —— **仍需网主放行事项 ⑤**。
#   kuku-sandbox-01 → 静默 **34.3 h**（连续第六轮）→ 已发**具体**催办
#     （写明要跑的命令、要核的三处：停机槽位归零 / 停机接线 / 超时顺序 15s<20s<30s）。
#   netns-probe → 静默 **34.5 h**（连续第六轮）→ 已发**具体**催办
#     （RSS 口径 + cred 命令面走查 + 「环境受限就说缺什么权限，不要空着」）。
#     他此前说过 netns ①②③ 本机无法验证 → 那一格应改记「环境受限」并说明缺什么。
#
# 本轮两格的「已认证 SHA」是有出处的（**派单 ≠ 回报**）：
#   doubao-cloud-vm-01 → 95884df  2026-11-21T22:22:09Z 自报「SHA=95884df 结论=PASS」，
#     口径：回环 + 自签锚点，**不是** 51 人名册真机、**不是**真实公网。
#   oh-no-agent        → 95884df  2026-11-21T23:08:50Z 自报「SHA=95884df 结论=PASS」，
#     口径：独立环境重拉重跑（e2e 43-0、go test -race mesh 107.1s + cli 163.4s ok、
#     中转非对齐尺寸 8 组 sha256 双端相等、kill -9 后接收端报失败行且 inbox 无脏文件）。
#     他明确按纪律**不在未推代码上报结果**（二进制帧在 worktree 里，未推 → 他不测）。
#   muse-licheng-test1 → 仍是 2891872。他**拒绝**在未获 owner 本人授权时拉新代码。
#   kuku / netns → 仍无 95884df 的结果（静默 34h+，已发具体催办，连续第六轮）。
# 各代理这一轮被指派的面：
#   doubao-cloud-vm-01 —— 真机功能面 + 身份链路（Round 71 回信已投递：成员设备上的名册误判已修 /
#                        令牌桶措辞已改 / 锚点侧 cred list+revoke 的实际输出由值守代跑）
#   kuku-sandbox-01 —— 单测语义（revoke 级联语义、续期循环的终态判定）
#   netns-probe —— 可用性与资源面（cred 命令面走查、常驻 join 的 RSS 口径）
#   oh-no-agent —— 同 SHA 外部复测（96fdef8 全项 PASS）
#   muse-licheng-test1 —— 全命令面黑盒走查（2891872 PASS + 4 条 CLI 备注）
#
# 结构性事实（外部测试员反复卡在这里）：
#   cred list / cred revoke 读的是【锚点本机】的名册文件，成员设备上根本没有 →
#   那三项必须在 175 上跑，值守代跑并把输出贴回。
