# design-prototypes · 设计原型归档

本目录只做**设计对照**，不是产品实现，也不是构建输入。

## 内容

- `aistudio-v1/` — AI Studio 生成的产品原型源码（React 19 + Vite + Three.js，
  四工作台视图 + 五场景演示数据模型）。**2026-11-12 网主验收结论：界面、交互、
  整体展示满意，予以保留**；工程架构（数据来源、引擎调用、平台打包）按
  [docs/architecture/APP-PACKAGING.md](../docs/architecture/APP-PACKAGING.md)
  重构，生产实现不直接复用本目录代码。
- `aistudio-v1-source.zip` — 网主交付的原始压缩包，原样保留以备对证。
- `cmesh-cockpit.html` — 更早的单文件视觉稿，参考用。

## 阅读注意（模板残留声明）

`aistudio-v1/` 由生成工具模板产出，其中部分声明与实际不符，已核实：

- `metadata.json` 与 `README.md` 宣称的 Gemini/AI 服务能力：源码中无对应调用，
  属模板占位描述；
- `package.json` 中的 `@google/genai`、`dotenv`、`motion`：锁文件存在但源码
  未导入使用；
- `.env.example`：占位值，无真实密钥；
- `index.html` 从 Google Fonts 拉取字体——离线/受限环境不可用，生产实现须换
  系统字体栈（与锚点面板的零外部依赖约束一致）。

数据全部来自 `src/data/` 演示 fixtures 与 `server.ts` 模拟接口，不代表真实
mesh 数据。任何生产入口不得依赖这些演示路径。
