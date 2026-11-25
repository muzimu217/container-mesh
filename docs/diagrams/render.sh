#!/usr/bin/env bash
# 把同目录下的 *.mmd 渲染成同名 *.png。
#
#   bash docs/diagrams/render.sh          # 全部
#   bash docs/diagrams/render.sh mesh     # 只渲染 mesh
#
# 改图请改 .mmd 再重新渲染，不要直接改 PNG。
#
# 踩过的两个坑：
# 1. 模板里用 f-string 拼 JS 时，`{` 会被 Python 当作占位符，
#    导致 `.catch(function(e){...})` 被吃掉、页面永远停在 loading。
#    所以下面用 replace('__SRC__', ...) 而不是 f-string。
# 2. 当 .mmd 源码包含会破坏行内 <script> 的字符时，注入仍会失败
#    （症状：105×62 的白图）。所以每次渲染后都跑一遍白图检查；
#    真遇到了，把 SRC 写进独立的 .js 文件、HTML 用 <script src> 加载。
set -euo pipefail
cd "$(dirname "$0")"

CHROME="${CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"

if [ $# -eq 0 ]; then set -- *.mmd; fi

for f in "$@"; do
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

  "$CHROME" --headless --disable-gpu --hide-scrollbars --window-size=1600,1400 \
    --screenshot="$n.png" --virtual-time-budget=10000 "file://$PWD/$n.html" 2>/dev/null
  rm -f "$n.html"

  # 渲染后必须检查：空白页或错误页也会产出一个 PNG，文件存在说明不了什么。
  if python3 -c 'import PIL' 2>/dev/null; then
    python3 - "$n.png" <<'PY'
import sys
from PIL import Image, ImageChops
im = Image.open(sys.argv[1]).convert('RGB')
bg = Image.new('RGB', im.size, (255, 255, 255))
bb = ImageChops.difference(im, bg).getbbox()
colors = len(im.getcolors(maxcolors=2000000) or [])
print(sys.argv[1], 'bbox', bb, 'colors', colors)
if bb is None or colors < 200:
    sys.exit('looks blank or errored (see the line above)')
PY
  else
    echo "$n.png: Pillow 未安装，跳过白图检查（pip install Pillow 后可自动检查）"
  fi
done
