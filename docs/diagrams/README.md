# 架构图

源文件是 Mermaid（`.mmd`），同名 `.png` 是渲染结果。**改图请改 `.mmd` 再重新渲染**，
不要直接改 PNG。

## 组网（阶段 2，当前设计）

| 图 | 回答的问题 |
|---|---|
| `mesh.png` | **网状拓扑**：N 台设备如何互联；实线=直连、虚线=经成员中转；发现节点只发成员信息 |
| `stack.png` | **协议栈四层**：身份 → 发现 → 连接 → 路由；橙色是新增，绿色是复用阶段 1 |

`stack.png` 里最关键的一点：**端到端加密层在路由层之上**。
中转节点只能解开「每一跳」的会话层，解不开「端到端」那一层，所以它转发的是密文。

## 阶段 1（已冻结，保留备查）

| 图 | 回答的问题 |
|---|---|
| `stage1-layers.png` | 加密与 mux 的顺序：为什么 mux 必须在 AEAD 之上 |
| `stage1-topo.png` | 谁需要入站端口：只有中继需要 |
| `stage1-flowL.png` | `-L` 的四道闸顺序：为什么策略必须在拨号之前 |

这三张图描述的是**阶段 1 的两人配对模型**。组网里拓扑变了，但
`stage1-layers.png` 里的分层约束（mux 在 AEAD 之上）**仍然成立**，因为组网复用了记录泵与 yamux。

---

## 重新渲染

```bash
cd docs/diagrams
for f in *.mmd; do
  n="${f%.mmd}"
  python3 - "$n" <<'PY'
import json, pathlib, sys
n = sys.argv[1]
src = pathlib.Path(f'{n}.mmd').read_text()
tpl = '''<!doctype html><html><head><meta charset="utf-8">
<style>html,body{margin:0;padding:0;background:#fff}
#out{display:inline-block;padding:24px;color:#000;font:14px monospace}</style>
<script src="https://cdn.jsdelivr.net/npm/mermaid@11/dist/mermaid.min.js"></script></head>
<body><div id="out">loading</div><script>
var SRC = __SRC__;
mermaid.initialize({startOnLoad:false, theme:'default',
  flowchart:{curve:'basis', padding:20, useMaxWidth:false}});
mermaid.render('g', SRC).then(function(r){
  document.getElementById('out').innerHTML = r.svg;
}).catch(function(e){
  document.getElementById('out').textContent = 'ERR ' + e.message;
});
</script></body></html>'''
pathlib.Path(f'{n}.html').write_text(tpl.replace('__SRC__', json.dumps(src)))
PY
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
    --headless --disable-gpu --hide-scrollbars --window-size=1600,1400 \
    --screenshot="$n.png" --virtual-time-budget=10000 "file://$PWD/$n.html" 2>/dev/null
  rm -f "$n.html"
done
```

**渲染后必须检查**，不能只看文件存在：

1. 裁剪空白（`ImageChops.difference` 取 bbox）
2. 确认颜色数 > 200 —— 空白页或错误页的颜色数会很低
3. 渲染失败时页面显示 `ERR ...`，截出来是白底黑字，容易被当成成功

```bash
python3 -c "
from PIL import Image, ImageChops
im = Image.open('mesh.png').convert('RGB')
bg = Image.new('RGB', im.size, (255,255,255))
bb = ImageChops.difference(im, bg).getbbox()
print('bbox', bb, 'colors', len(im.getcolors(maxcolors=2000000) or []))
"
```

> 踩过的两个坑：
> 1. 模板里用 f-string 拼 JS 时，`{` 会被 Python 当作占位符，
>    导致 `.catch(function(e){...})` 被吃掉、页面永远停在 `loading`。
> 2. 即使改用 `tpl.replace('__SRC__', ...)`，当 `.mmd` 源码包含会破坏
>    行内 `<script>` 的字符时注入仍会失败（症状：105×62 的白图）。
>
> 可靠做法是**把 SRC 写进独立的 `.js` 文件**、HTML 用 `<script src>` 加载，
> 渲染完删除临时文件。上面的循环脚本已按此更新。
