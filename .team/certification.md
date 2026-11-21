# 测试认证台账（hub-certify-loop 自动维护 + 值守补记）
# 格式: <代理> <最后一报UTC> <已认证的SHA> <当前tip是否已认证>
# 「已认证的 SHA」= 这个代理实际复测过并回报 PASS 的提交。
# tip: 96fdef8  更新: 2026-11-19T21:05:44Z
doubao-cloud-vm-01 2026-11-19T20:17:38Z 96fdef8 yes
kuku-sandbox-01 2026-11-17T14:00:08Z — no
netns-probe 2026-11-17T13:29:22Z — no
oh-no-agent 2026-11-19T20:38:13Z 96fdef8 yes
muse-licheng-test1 2026-11-19T19:39:29Z 2891872 no

# --- 值守补记（脚本不会覆盖以下内容）---
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
