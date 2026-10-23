# 架构图

源文件是 Mermaid（`.mmd`），同名 `.png` 是渲染结果。改图请改 `.mmd` 再重新渲染，
不要直接改 PNG。

## 重新渲染

```bash
cd docs/diagrams
for f in *.mmd; do
  n="${f%.mmd}"
  python3 - "$n" <<'PY'
import json, pathlib, sys
n = sys.argv[1]
src = pathlib.Path(f'{n}.mmd').read_text()
html = f'''<!doctype html><html><head><meta charset="utf-8">
<style>body{{margin:0;padding:24px;background:#fff;font-family:-apple-system,sans-serif}}</style>
<script src="https://cdn.jsdelivr.net/npm/mermaid@11/dist/mermaid.min.js"></script></head>
<body><div id="out"></div><script>
mermaid.initialize({{startOnLoad:false,theme:'default',flowchart:{{curve:'basis',padding:16}}}});
mermaid.render('g', {json.dumps(src)}).then(r=>{{document.getElementById('out').innerHTML=r.svg;}});
</script></body></html>'''
pathlib.Path(f'{n}.html').write_text(html)
PY
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
    --headless --disable-gpu --hide-scrollbars --window-size=1400,1000 \
    --screenshot="$n.png" --virtual-time-budget=8000 "file://$PWD/$n.html" 2>/dev/null
  rm -f "$n.html"
done
```

## 三张图分别回答什么

| 图 | 回答的问题 |
|---|---|
| `layers.png` | **加密与多路复用的顺序**：为什么 mux 必须在 AEAD 之上 |
| `topo.png` | **谁需要入站端口**：只有中继需要，客户端全部零入站 |
| `flowL.png` | **`-L` 转发的四道闸顺序**：为什么策略必须在拨号之前 |
