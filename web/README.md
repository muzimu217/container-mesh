# cmesh 驾驶舱（cockpit）

3D 名册视图的前端源码。构建产物被 `//go:embed` 进 cmesh 二进制，
由锚点的 `serve-discovery --panel` 在 `/panel` 提供；轻量版在 `/panel-lite`。

## 构建

```bash
cd web
npm ci
npm run build          # 产物写到 web/dist
```

把 `web/dist/` 的内容同步到 `internal/relay/cockpit-dist/`，再 `go build ./...`：

```bash
rm -rf ../internal/relay/cockpit-dist && cp -R dist ../internal/relay/cockpit-dist
```

（`internal/relay/cockpit-dist/` 是入库的构建产物——发布件必须自带面板，
不能要求部署机器装 Node。）

## 开发

```bash
npm run dev            # 本地起 vite，默认 5173
```

开发服务器需要一份 `/api/mesh` 的数据才能渲染名册。两种方式：

- 对着一个真实锚点：在页面弹出的令牌框里填面板令牌，页面会直连
  `/api/mesh`（需要 vite 代理到锚点，见 `vite.config.ts`）。
- 不连锚点：页面显示「未拿到名册」，不会编造设备——这是刻意的，
  驾驶舱显示的每一个数字都必须来自真实数据。

## 约束

- **零外部引用**：不许引 CDN、web font、外部图片。面板要在没有外网的容器里渲染。
  测试 `TestCockpitAssetsAreLocal` / `TestCockpitFetchesOnlyOurEndpoint` 会卡这条。
- **不编造**：地理、运行时指标 cmesh 不采集，就写「未登记」「未采集」。
  不许用演示数据填充（`TestCockpitHasNoDemoControls`）。
- 令牌只走 `X-Panel-Token` 请求头，不落 URL。
