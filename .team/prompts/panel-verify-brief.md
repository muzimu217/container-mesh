# 验收单 · 可视化面板 v1（测试代理用）

被测：develop 最新 HEAD（届时公告给出 SHA）。前提：你环境内搭好最小拓扑
（serve-discovery + ≥1 成员，参考 deploy/MESH-NET.md §3/§7）。

## 检查项（逐条给数/给截图描述）

1. `GET /panel` 返回 200 且为完整 HTML（无外链 JS/CSS/字体——grep 源码确认）
2. `GET /api/mesh` JSON 与你拓扑真实一致：members 数 = 你的设备数；online 数
   随成员启停**自动变化**（kill 一个成员，≤10s 内面板在线数-1）
3. 断网形态：停掉发现节点 → 面板出现连接失败提示且**不白屏**；重启节点 → 自动恢复
4. 大数字（online/members）与成员表逐字段核对：device_id / 端点 / 相对时间 / anchor 徽标
5. 离线容器可用性：在无外网出口的 netns 里起 serve-discovery，页面仍完整渲染
   （零 CDN 依赖的实证）
6. `Network` 面板：除 `/api/mesh` 轮询外无任何出站请求

## 回报

按提交规范回 hub（topic=issue8）：每项 PASS/FAIL + 证据（curl 输出、面板截图描
述、时间线）。不判项单列。
