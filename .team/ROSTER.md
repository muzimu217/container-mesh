# 工作团：代理 × 模型 × 角色 × 状态

> 每个代理的模型都是**实测确认**的——问代理本身、查它的启动日志、或读它的配置文件。
> 没有实测到的一律标「未确认」，不猜。
>
> 实测日期：2026-10-31。模型会随供应商更新漂移，**每隔几轮迭代应重新核一次**。

---

## 1. 总表

| # | 代理 | 命令 | 模型（实测） | 提供商 | 可用性 | 团队角色 |
|---|---|---|---|---|---|---|
| 1 | claude | `claude -p "…"` | **Claude Opus 5**（1M 上下文） | Anthropic | ✅ | **安全评审 + 性能分析**（推理最重的两个岗） |
| 2 | qoder | `qoder -p "…"` | **Qwen3.8-Flash** | 阿里通义（Qoder 托管） | ✅ | **QA 评审** |
| 3 | codebuddy | `codebuddy -p "…"` | **Hy4 preview** | 腾讯 | ✅ | **实现评审** |
| 4 | codearts | `codearts run "…"` | **DeepSeek v4.1-flash** | 华为云 CodeArts | ✅ | **文档机械核对** |
| 5 | opencode | `opencode run "…"` | big-pickle | OpenCode 自有 | ⚠️ **免费额度已耗尽** | 需求批判（额度恢复后复用） |
| 6 | codex | `codex exec --skip-git-repo-check "…"` | GPT-6.1-sol（经 agentrouter.org 中转） | OpenAI（中转） | ⚠️ 网络重连循环 | 备份全岗（恢复后接入） |
| 7 | agy | `agy -p "…"` | Gemini 3.8 Flash / Claude Sonnet 4.6 / GPT-OSS 120B（可切） | Google AI Pro | ❌ **地区封锁确认**（2026-10-31 实测：CN/HK 出口均被拒，本机与两台云服务器全部出口为 HK/CN，无可用节点；认证墙+地区墙双重阻塞） | **暂不可用**（除非接入受支持地区的出口） |
| 8 | ZCode（我） | 本会话 | GLM 套餐 | Z.ai | ✅ | **产品经理 + 首席协调 + 主写手** |

---

## 2. 模型实测方法（可复现）

不同代理报告模型的方式不同，逐个试出来的路径：

| 代理 | 方法 | 结果来源 |
|---|---|---|
| claude | 直接问「What model are you?」 | 自报 "Opus 5 (1M context)" |
| codebuddy | 问「Are you Claude/GPT/GLM/…?」 | 自报 "Hy4 preview model, not Claude/GPT/GLM/DeepSeek/hunyuan" |
| codearts | 启动行自带模型标签 | `> build · deepseek-v4.1-flash` |
| opencode | 查运行日志 | `~/.local/share/opencode/log/opencode.log` → `llm.model=big-pickle`；且日志显示 **free limit reached** |
| qoder | 问它不答（拒绝披露）→ 查启动日志 | `~/.qoder/logs/latest/` → `"model":"Qwen3.8-Flash"` |
| codex | 读配置文件 | `~/.codex/config.toml` → `model = "gpt-6.1-sol"`、`base_url = agentrouter.org` |
| agy | `agy models` 列出全部可选 | 三档可切 |

**教训**：qoder 拒绝自报模型，但它的运行日志写了；opencode 自报了但日志暴露了额度耗尽。**「代理说了什么」不如「日志记了什么」可信。**

---

## 3. 角色与模型的匹配理由

| 角色 | 分给了谁 | 为什么是这个模型 |
|---|---|---|
| **安全评审** | claude（Opus 5） | 安全推理是全团最重的岗：本轮实战中它发现了 ed25519 panic、控制字符注入、Register 无认证三项真问题，且全部附 file:line。Opus 的长上下文也能一次读完整个包 |
| **性能分析** | claude（兼） | 同为推理密集：分配/锁/拷贝分析需要跨文件追踪 |
| **QA 评审** | qoder（Qwen3.8-Flash） | 测试有效性评审的核心是「破坏方向对不对、环境真不真」，模式化程度高，flash 档够用 |
| **实现评审** | codebuddy（Hy4） | 独立视角读结构与抽象，非推理密集 |
| **文档核对** | codearts（DeepSeek v4.1-flash） | 纯机械：链接、编号、数字比对。实战已抓到 5 处真实不一致 |
| **需求批判** | opencode（big-pickle） | 额度恢复后复用；目前由 claude 兼 |
| **备份全岗** | codex（GPT-6.1-sol） | 网络恢复后接入；agentrouter 中转的稳定性待观察 |

---

## 4. 委派命令速查（复制即用）

```bash
# 安全评审（claude）
timeout 300 claude -p "$(cat .team/prompts/security-review.md | sed 's|@@TARGET@@|internal/discovery/|g; s|@@ROUND@@|Round 5|g')"

# 实现评审（codebuddy）
timeout 300 codebuddy -p "$(cat .team/prompts/impl-review.md | sed 's|@@TARGET@@|internal/identity/|g; s|@@ROUND@@|Round 5|g')"

# QA 评审（qoder）
timeout 300 qoder -p "$(cat .team/prompts/qa-review.md | sed 's|@@TARGET@@|internal/discovery/*_test.go|g; s|@@ROUND@@|Round 5|g')"

# 文档核对（codearts）
timeout 300 codearts run "$(cat .team/prompts/doc-check.md)"

# 需求批判（opencode，额度恢复后）
timeout 300 opencode run "$(cat .team/prompts/req-critique.md | sed 's|@@ROUND@@|Round 5|g')"

# 备份（codex，网络恢复后）
timeout 300 codex exec --skip-git-repo-check "$(cat .team/prompts/security-review.md | sed 's|@@TARGET@@|internal/discovery/|g')"
```

---

## 5. 已知问题与处理

| 问题 | 影响 | 处置 |
|---|---|---|
| opencode 免费额度耗尽 | 需求批判岗缺人 | claude 兼；或等额度恢复（日志显示按月重置） |
| codex 网络重连循环 | 备份岗缺人 | agentrouter 中转不稳；监测，恢复后接入 |
| agy 认证超时 | 无法使用 | 需美/日/新出口或交互式登录；不阻塞 |
| qoder 不披露模型 | 已从日志确认 | 无影响 |
| 模型漂移 | 表格过期 | **每 3 轮迭代重新核一次**（把本文件 §1 更新） |

---

## 6. 与 TEAM.md 的关系

TEAM.md 的六道**虚拟门**不变——它们是合并判据。本文只决定「每道门**请谁**来站」：

| TEAM.md 虚拟角色 | 本文真实代理 | 冗余 |
|---|---|---|
| ①首席协调 | ZCode（我） | — |
| ②需求专家 | opencode / claude 兼 | 无独立冗余 |
| ③开发专家 | 我（写）+ codebuddy（审） | 读写分离 |
| ④测试专家 | 我（写测试）+ qoder（审测试） | 读写分离 |
| ⑤安全专家 | claude | 独立第三方 |
| ⑥性能专家 | claude（不同提示词模板） | 与安全共享但有独立模板 |
