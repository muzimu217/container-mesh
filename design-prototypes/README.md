# design-prototypes · 设计原型归档

本目录只做**设计对照与溯源**，不是构建输入——构建输入是 [web/](../web/)。

## 内容

- `aistudio-v1/` — 网主交付的原型源码（React 19 + Vite + Three.js，
  四工作台视图 + 五场景演示数据模型）。**2026-11-12 网主验收：界面、交互、
  整体展示满意**。生产实现 [web/](../web/) 即由本目录派生：界面代码照用，
  演示数据与模拟接口换成 `GET /api/mesh` 实时名册。
- `aistudio-v1-source.zip` — 网主交付的原始压缩包，原样保留以备对证
  （sha256 `a6bb87b7…`，与 `web/` 的来源一致）。
- `cmesh-cockpit.html` — 更早的单文件视觉稿，**已被 `web/` 取代，未随二进制发布**。

## 阅读注意（模板残留声明）

`aistudio-v1/` 由生成工具模板产出，其中部分声明与实际不符，已核实：

- `metadata.json` 与 `README.md` 宣称的 Gemini/AI 服务能力：源码中无对应调用，
  属模板占位描述；
- `package.json` 中的 `@google/genai`、`dotenv`、`motion`：锁文件存在但源码
  未导入使用；
- `.env.example`：占位值，无真实密钥；
- `index.html` 从 Google Fonts 拉取字体——离线/受限环境不可用。

**以上四项在 `web/` 里已经全部清掉**：无 Gemini 依赖、无 `.env` 模板、
无 `server.ts` 模拟接口、字体走系统栈。`web/` 只保留渲染所需的运行时依赖。

数据全部来自 `src/data/` 演示 fixtures 与 `server.ts` 模拟接口，不代表真实
mesh 数据。任何生产入口不得依赖这些演示路径——`TestCockpitHasNoDemoControls`
会扫构建产物，卡住演示机制的回归。
