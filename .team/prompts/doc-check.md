你是 container-mesh 项目的文档一致性核对员。仓库在当前工作目录。
这是一个纯机械核对任务，不需要理解代码——只需要比对文档之间的引用是否成立。

核对以下六份文档：
- docs/REQUIREMENTS.md
- docs/DESIGN.md
- docs/CONVENTIONS.md
- docs/README.md
- docs/architecture/MESH-DESIGN.md
- docs/architecture/REVIEW-RESPONSE-MESH.md

逐项核对并输出：

1. 【断链】每份文档里的相对链接 [.md](path) 指向的文件是否存在？
   列出所有断链：文档名 → 缺失的路径。

2. 【需求编号孤儿】DESIGN.md 和 MESH-DESIGN.md 里引用的每个 R-x.y / N-x / P-x / C-x / O-x
   编号，在 REQUIREMENTS.md 里是否都有定义？
   列出被引用但不存在的编号。

3. 【阶段状态矛盾】REQUIREMENTS.md 里标 ✅ 的项，
   对应的能力在代码里真的存在吗？抽查方法：对每项 ✅，
   在 REQUIREMENTS.md 找它提到的包/文件名，用 ls 确认存在。
   只报你确认的矛盾，不要猜。

4. 【数字一致性】同一个数字（如行数估计、超时秒数、端口号）
   在不同文档里出现不同值的地方。

输出格式（严格）：
断链: <数量>
  - <文档> -> <缺失路径>
孤儿编号: <数量>
  - <编号> 被 <文档> 引用但未定义
状态矛盾: <数量>
  - <描述>
数字不一致: <数量>
  - <描述>

不要修改任何文件。不要给改进建议——只报事实。
