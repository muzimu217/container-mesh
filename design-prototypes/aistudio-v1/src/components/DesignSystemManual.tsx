import React, { useState } from 'react';
import { ScenarioKey } from '../data/meshSnapshots';
import {
  CheckCircle2,
  XCircle,
  Layers,
  Palette,
  Cpu,
  Smartphone,
  Shield,
  GitBranch,
  Terminal,
  BookOpen,
} from 'lucide-react';

interface DesignSystemManualProps {
  onApplyScenarioAndJump: (scenario: ScenarioKey) => void;
}

export const DesignSystemManual: React.FC<DesignSystemManualProps> = ({
  onApplyScenarioAndJump,
}) => {
  const [activeSection, setActiveSection] = useState<string>('all');

  const navItems = [
    { id: 'all', label: '全部 12 项规范总览' },
    { id: 'sec1_3', label: '01–03. 视觉方向·3D风格·信息架构' },
    { id: 'sec4_6', label: '04–06. 设计系统·组件·四维状态矩阵' },
    { id: 'sec7_9', label: '07–09. 3D节点规范·五种状态·中文文案' },
    { id: 'sec10_12', label: '10–12. 跨端适配·未来扩展·事实边界' },
  ];

  const showSec = (group: string) => activeSection === 'all' || activeSection === group;

  return (
    <div className="space-y-8 pb-12">
      {/* Header & Section Filter Bar */}
      <div className="bg-white border border-slate-200 rounded-lg p-6">
        <div className="flex flex-col md:flex-row md:items-center justify-between gap-4 border-b border-slate-200 pb-5">
          <div>
            <div className="text-xs font-mono text-sky-700 font-semibold">
              container-mesh / cmesh · DESIGN SPECIFICATION MANUAL (12 DELIVERABLES)
            </div>
            <h1 className="text-2xl font-bold text-slate-900 mt-1">
              cmesh 桌面客户端与 3D 嵌套组网控制台 — 12 项完整设计规范与高保真原型规划
            </h1>
            <p className="text-sm text-slate-600 mt-1.5 max-w-3xl">
              面向 Windows / macOS 桌面应用及移动端扩展的用户态私有组网客户端规范。遵循“像一台研究仪器一样严谨，像一个日常工具一样好用”的准则，采用“机房分区 → 容器节点 → 底层配置”三级嵌套信息架构。
            </p>
          </div>
          <div className="shrink-0">
            <button
              onClick={() => onApplyScenarioAndJump('normal')}
              className="px-4 py-2 bg-slate-900 hover:bg-slate-800 text-white text-xs font-medium rounded-md transition-colors cursor-pointer whitespace-nowrap"
            >
              返回 3D 驾驶舱实时体验 →
            </button>
          </div>
        </div>

        {/* Interactive Chapter Filter Tabs */}
        <div className="flex flex-wrap items-center gap-1.5 pt-4">
          {navItems.map((item) => (
            <button
              key={item.id}
              onClick={() => setActiveSection(item.id)}
              className={`px-3 py-1.5 rounded-md text-xs font-medium transition-colors cursor-pointer ${
                activeSection === item.id
                  ? 'bg-sky-700 text-white'
                  : 'bg-slate-100 text-slate-600 hover:text-slate-900'
              }`}
            >
              {item.label}
            </button>
          ))}
        </div>
      </div>

      {/* ================= SECTIONS 01 - 03 ================= */}
      {showSec('sec1_3') && (
        <div className="grid grid-cols-1 lg:grid-cols-3 gap-6">
          {/* 01. 产品视觉方向说明 */}
          <div className="bg-white border border-slate-200 rounded-lg p-5 space-y-3">
            <div className="flex items-center justify-between text-xs font-mono text-slate-500">
              <span>01. VISUAL DIRECTION</span>
              <BookOpen className="w-4 h-4 text-sky-600" />
            </div>
            <h2 className="text-base font-bold text-slate-900">01. 产品视觉方向说明</h2>
            <p className="text-xs text-slate-600 leading-relaxed">
              定位为<strong>跨平台桌面原生组网客户端（Windows / macOS，后续延伸至 iOS / Android）</strong>，而非营销型外部网站。整体气质聚焦于：
              <strong className="text-slate-900">私有、自主、可靠、清晰、可验证、安静耐看</strong>。
            </p>
            <ul className="space-y-1.5 text-xs text-slate-700 pt-1">
              <li>· <strong>学术派仪器感</strong>：所有读数来源可追溯（关联 `/api/mesh` 的 `roster_version` 与 `generated_at`），术语恒定，异常状态给出明确原因。</li>
              <li>· <strong>哑光白实验室基底</strong>：采用 `#F8FAFC` 台面画布、`#FFFFFF` 仪器面板、半透明光学玻璃罩与拉丝金属构件，配合青蓝色（`#0284C7`）信号导轨。</li>
              <li>· <strong>嵌套式渐进披露（Progressive Nesting）</strong>：主界面呈现机房与容器间的宏观连通态势；仅当用户点击机房或具体容器时，才逐层下钻显示底层运行时、零特权标志与远程操作台。</li>
            </ul>
          </div>

          {/* 02. 3D 网络驾驶舱的整体风格 */}
          <div className="bg-white border border-slate-200 rounded-lg p-5 space-y-3">
            <div className="flex items-center justify-between text-xs font-mono text-slate-500">
              <span>02. 3D COCKPIT STYLE</span>
              <Layers className="w-4 h-4 text-sky-600" />
            </div>
            <h2 className="text-base font-bold text-slate-900">02. 3D 网络驾驶舱的整体风格</h2>
            <p className="text-xs text-slate-600 leading-relaxed">
              采用 <strong>Blender Cycles 级半写实等距（Isometric）3D 技术插画</strong>语言，服务于空间层级理解与状态判读，绝不喧宾夺主：
            </p>
            <div className="space-y-2 text-xs">
              <div className="p-2.5 bg-emerald-50/60 border border-emerald-200 rounded text-emerald-950">
                <div className="font-semibold mb-1">推荐表现（已实现于原型）：</div>
                <div>等距实验室基准台面 · 三大机房亚克力防护罩 · 7 部件复合容器节点 · 区分直连/中继/离线的抛物线通信轨道 · 可拆解层级滑块。</div>
              </div>
              <div className="p-2.5 bg-slate-50 border border-slate-200 rounded text-slate-700">
                <div className="font-semibold text-slate-900 mb-1">严格禁止（反浮夸准则）：</div>
                <div>无赛博朋克霓虹海洋 · 无虚假星空与彩色粒子云 · 无遮挡视线的游戏 HUD · 无缺少真实数据支撑的随机拓扑线 · 无导致无法阅读的连续自转。</div>
              </div>
            </div>
          </div>

          {/* 03. 页面信息架构 */}
          <div className="bg-white border border-slate-200 rounded-lg p-5 space-y-3">
            <div className="flex items-center justify-between text-xs font-mono text-slate-500">
              <span>03. INFORMATION ARCHITECTURE</span>
              <GitBranch className="w-4 h-4 text-sky-600" />
            </div>
            <h2 className="text-base font-bold text-slate-900">03. 客户端页面信息架构（三级嵌套）</h2>
            <div className="text-xs font-mono bg-slate-900 text-slate-100 p-3 rounded leading-relaxed overflow-x-auto">
              {`[1. 桌面客户端顶栏] 品牌 · 网络名 · 账号ID · 视图与态势切换
[2. 核心遥测通栏]   "4 / 5 在线设备" · 名册版本 · 同步状态 · 锚点与传输模式
[3. 主工作区 (左 3D 嵌套视口 + 右 渐进式检视器)]
    ├─ Level 1 (默认总览): 3大机房区整体状态 + 容器间连通关系
    ├─ Level 2 (点击机房): 机房光学护罩抬升 + 区域内嵌容器名册
    └─ Level 3 (点击容器): 四维状态矩阵 + 端点 + 嵌套底层配置 + 受控操作
[4. 按需诊断抽屉]   默认收起，展开后显示失联原因、重试状态與缓存说明`}
            </div>
          </div>
        </div>
      )}

      {/* ================= SECTIONS 04 - 06 ================= */}
      {showSec('sec4_6') && (
        <div className="space-y-6">
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
            {/* 04. 设计系统 (色彩、字体、间距、语义) */}
            <div className="bg-white border border-slate-200 rounded-lg p-5 space-y-4">
              <div className="flex items-center justify-between text-xs font-mono text-slate-500">
                <span>04. DESIGN SYSTEM TOKENS</span>
                <Palette className="w-4 h-4 text-sky-600" />
              </div>
              <h2 className="text-base font-bold text-slate-900">
                04. 设计系统（色彩、字体、间距与三重编码规则）
              </h2>
              <p className="text-xs text-slate-600">
                核心铁律：<strong>绝不单独依靠颜色表达状态</strong>，每个状态同时具备「语义色彩 + 几何符号（● / ◆ / ○ / ▲ / ✖）+ 明确中文短语」。
              </p>

              {/* Color Swatches */}
              <div className="grid grid-cols-2 sm:grid-cols-3 gap-2.5 text-xs">
                <div className="p-2.5 border border-slate-200 rounded bg-[#F8FAFC]">
                  <div className="w-full h-5 rounded bg-[#F8FAFC] border border-slate-300 mb-1.5" />
                  <div className="font-semibold text-slate-900">哑光白实验台</div>
                  <div className="font-mono text-[11px] text-slate-500">#F8FAFC · 60% 主画布</div>
                </div>
                <div className="p-2.5 border border-slate-200 rounded bg-white">
                  <div className="w-full h-5 rounded bg-[#0284C7] mb-1.5" />
                  <div className="font-semibold text-slate-900">◈ 网络核心/直连青蓝</div>
                  <div className="font-mono text-[11px] text-slate-500">#0284C7 · 锚点与P2P轨</div>
                </div>
                <div className="p-2.5 border border-slate-200 rounded bg-white">
                  <div className="w-full h-5 rounded bg-[#059669] mb-1.5" />
                  <div className="font-semibold text-slate-900">● 在线可达翠绿</div>
                  <div className="font-mono text-[11px] text-slate-500">#059669 · 仅表当前可达</div>
                </div>
                <div className="p-2.5 border border-slate-200 rounded bg-white">
                  <div className="w-full h-5 rounded bg-[#64748B] mb-1.5" />
                  <div className="font-semibold text-slate-900">○ 离线在册中性灰</div>
                  <div className="font-mono text-[11px] text-slate-500">#64748B · 离线非事故</div>
                </div>
                <div className="p-2.5 border border-slate-200 rounded bg-white">
                  <div className="w-full h-5 rounded bg-[#D97706] mb-1.5" />
                  <div className="font-semibold text-slate-900">◆ 等待/无端点/缓存琥珀</div>
                  <div className="font-mono text-[11px] text-slate-500">#D97706 · 需关注或缓存</div>
                </div>
                <div className="p-2.5 border border-slate-200 rounded bg-white">
                  <div className="w-full h-5 rounded bg-[#DC2626] mb-1.5" />
                  <div className="font-semibold text-slate-900">✖ 鉴权拒绝/致命错误红</div>
                  <div className="font-mono text-[11px] text-slate-500">#DC2626 · 仅限真正错误</div>
                </div>
              </div>

              <div className="pt-2 border-t border-slate-100 text-xs text-slate-600 space-y-1">
                <div>
                  <strong>字体规范</strong>：标题 `Plus Jakarta Sans` · 正文 `Public Sans` / 系统无衬线 · 读数/ID/端点强制 `JetBrains Mono (tabular-nums)`。
                </div>
                <div>
                  <strong>圆角与阴影</strong>：外层容器 `rounded-lg (8px)`，内嵌槽位 `rounded-md (6px)`，统一采用 `1px solid #E2E8F0` 精密细线边框替代浮夸弥散阴影。
                </div>
              </div>
            </div>

            {/* 05. 组件清单 */}
            <div className="bg-white border border-slate-200 rounded-lg p-5 space-y-3">
              <div className="flex items-center justify-between text-xs font-mono text-slate-500">
                <span>05. COMPONENT INVENTORY</span>
                <Cpu className="w-4 h-4 text-sky-600" />
              </div>
              <h2 className="text-base font-bold text-slate-900">05. 核心桌面客户端组件清单（10 大标准组件）</h2>
              <div className="divide-y divide-slate-100 text-xs">
                {[
                  ['C-01 客户端窗口顶栏 (Client Header)', '承载品牌、当前私有网络名、账号短 ID、只读/管理模式与工作区切换。'],
                  ['C-02 首屏核心读数锚点 (Online Hero Readout)', '醒目展示 `4 / 5 在线设备`、名册版本 `v42`、锚点状态与主传输模式。'],
                  ['C-03 发现节点失联/缓存横幅 (Snapshot Alert Banner)', '当发现节点失联时以琥珀色提示“当前显示缓存数据 (时间)”及自动重试倒计时。'],
                  ['C-04 三级空间层级面包屑 (Spatial Breadcrumb)', '支持 `全网总览 -> 机房区域 -> 容器节点` 的嵌套定位与一键返回上级。'],
                  ['C-05 3D 等距机房与容器视图 (Isometric 3D Stage)', '含可抬升机房玻璃罩、7 部件复合容器模型、状态光环与通信脉冲轨迹。'],
                  ['C-06 底部可收起仪器坞 (Collapsible Bottom Dock)', '提供等距/俯视视角切换、机房罩拆解滑块、标签与通信轨迹开关。'],
                  ['C-07 四维正交状态读取卡 (4D State Matrix Card)', '解耦展示 Membership、Liveness、Announcement、Discovery 四项独立状态。'],
                  ['C-08 嵌套底层容器配置抽屉 (Nested Config Accordion)', '默认折叠在容器内部，点开后展示运行时、零 root、零 CAP_NET_ADMIN 详情。'],
                  ['C-09 受控用户态操作台 (Userspace Action Workbench)', '支持 RTT 测量、点对点发消息、大文件传输校验、白名单命令执行。'],
                  ['C-10 按需网络诊断抽屉 (On-Demand Diagnostics)', '默认收起，避免原始日志刷屏；展开后解释节点不可达原因与同步状态。'],
                ].map(([title, desc], i) => (
                  <div key={i} className="py-2 flex flex-col sm:flex-row sm:items-center justify-between gap-1">
                    <span className="font-semibold text-slate-900 shrink-0">{title}</span>
                    <span className="text-slate-600 sm:text-right">{desc}</span>
                  </div>
                ))}
              </div>
            </div>
          </div>

          {/* 06. 设备状态矩阵 (Four-Dimensional State Matrix) */}
          <div className="bg-white border border-slate-200 rounded-lg p-5 space-y-3">
            <div className="text-xs font-mono text-slate-500">06. ORTHOGONAL DEVICE STATE MATRIX</div>
            <h2 className="text-base font-bold text-slate-900">
              06. 设备四维正交状态矩阵（Membership × Liveness × Announcement × Discovery）
            </h2>
            <div className="overflow-x-auto">
              <table className="w-full text-left border-collapse text-xs">
                <thead>
                  <tr className="border-b border-slate-200 bg-slate-50 text-slate-700 font-semibold">
                    <th className="py-2.5 px-3">典型设备场景</th>
                    <th className="py-2.5 px-3">1. Membership (名册)</th>
                    <th className="py-2.5 px-3">2. Liveness (在线)</th>
                    <th className="py-2.5 px-3">3. Announcement (端点)</th>
                    <th className="py-2.5 px-3">4. 连接类型 (Transport)</th>
                    <th className="py-2.5 px-3">三重编码视觉标识</th>
                    <th className="py-2.5 px-3">界面准确解释（消除误解）</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-slate-200 text-slate-700">
                  <tr>
                    <td className="py-2.5 px-3 font-semibold text-slate-900">自托管发现锚点 (`anchor-hk-01`)</td>
                    <td className="py-2.5 px-3 font-mono text-emerald-700">In Roster (v42)</td>
                    <td className="py-2.5 px-3 font-mono text-emerald-700">Online</td>
                    <td className="py-2.5 px-3 font-mono">2 个公网端点</td>
                    <td className="py-2.5 px-3 font-mono text-sky-700">direct (Anchor)</td>
                    <td className="py-2.5 px-3 font-mono text-sky-700">◈ 在线 · 核心锚点</td>
                    <td className="py-2.5 px-3">用户自托管信任中心，负责名册同步与受限环境中继兜底。</td>
                  </tr>
                  <tr>
                    <td className="py-2.5 px-3 font-semibold text-slate-900">在线直连容器 (`gpu-trainer-02`)</td>
                    <td className="py-2.5 px-3 font-mono text-emerald-700">In Roster (v42)</td>
                    <td className="py-2.5 px-3 font-mono text-emerald-700">Online</td>
                    <td className="py-2.5 px-3 font-mono">已公告可拨地址</td>
                    <td className="py-2.5 px-3 font-mono text-emerald-700">direct (P2P)</td>
                    <td className="py-2.5 px-3 font-mono text-emerald-700">● 在线 · 直连</td>
                    <td className="py-2.5 px-3">数据面直接通过用户态 UDP 加密互通，不经过锚点中转。</td>
                  </tr>
                  <tr>
                    <td className="py-2.5 px-3 font-semibold text-slate-900">无端点受限沙箱 (`ci-runner-ephemeral`)</td>
                    <td className="py-2.5 px-3 font-mono text-emerald-700">In Roster (v42)</td>
                    <td className="py-2.5 px-3 font-mono text-emerald-700">Online</td>
                    <td className="py-2.5 px-3 font-mono text-amber-700">无 (`endpoints: []`)</td>
                    <td className="py-2.5 px-3 font-mono text-amber-700">pending (仅出站)</td>
                    <td className="py-2.5 px-3 font-mono text-amber-700">◆ 在册 · 无公告端点</td>
                    <td className="py-2.5 px-3">已属于网络，但未开启 `--listen` 或刚获批尚未完成首次公告。</td>
                  </tr>
                  <tr>
                    <td className="py-2.5 px-3 font-semibold text-slate-900">CGNAT 锚点中继节点</td>
                    <td className="py-2.5 px-3 font-mono text-emerald-700">In Roster (v43)</td>
                    <td className="py-2.5 px-3 font-mono text-emerald-700">Online</td>
                    <td className="py-2.5 px-3 font-mono text-amber-700">无公网直连端点</td>
                    <td className="py-2.5 px-3 font-mono text-amber-700">relay (锚点转发)</td>
                    <td className="py-2.5 px-3 font-mono text-amber-700">⇄ 在线 · 锚点中继</td>
                    <td className="py-2.5 px-3">无公网入站条件，当前经由自托管锚点安全中继通信。</td>
                  </tr>
                  <tr>
                    <td className="py-2.5 px-3 font-semibold text-slate-900">离线归档设备 (`home-nas-vault`)</td>
                    <td className="py-2.5 px-3 font-mono text-emerald-700">In Roster (仍在册)</td>
                    <td className="py-2.5 px-3 font-mono text-slate-500">Offline (不可达)</td>
                    <td className="py-2.5 px-3 font-mono text-slate-400">保留历史端点</td>
                    <td className="py-2.5 px-3 font-mono text-slate-500">unavailable</td>
                    <td className="py-2.5 px-3 font-mono text-slate-500">○ 离线（仍在名册中）</td>
                    <td className="py-2.5 px-3">离线不等于被删除，也不是红色安全事故；恢复供电后自动重连。</td>
                  </tr>
                </tbody>
              </table>
            </div>
          </div>
        </div>
      )}

      {/* ================= SECTIONS 07 - 09 ================= */}
      {showSec('sec7_9') && (
        <div className="space-y-6">
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
            {/* 07. 3D 节点与连接线规范 */}
            <div className="bg-white border border-slate-200 rounded-lg p-5 space-y-3">
              <div className="text-xs font-mono text-slate-500">07. 3D NODE & TRACK SPECIFICATION</div>
              <h2 className="text-base font-bold text-slate-900">07. 3D 嵌套机房、容器节点与连接线几何规范</h2>
              <ul className="space-y-2 text-xs text-slate-700 leading-relaxed">
                <li>
                  <strong>外层机房基座（Zone Bay Group）</strong>：由哑光白精密底座、阳极氧化铝倒角边框、4 根四角标尺立柱、可平滑抬升的半透明光学玻璃护罩（Nested Glass Canopy）构成。点击机房或拖动拆解滑块时，护罩向上分离，露出内部嵌套容器。
                </li>
                <li>
                  <strong>内层容器节点（7-Part Composite Node）</strong>：
                  <ol className="list-decimal list-inside pl-2 mt-1 space-y-0.5 text-slate-600">
                    <li>深灰金属法兰安装底盘（Flange Pedestal）</li>
                    <li>语义状态发光环（Status Torus：翠绿在线 / 琥珀无端点或中继 / 石板灰离线）</li>
                    <li>容器主体核心（锚点为八棱柱核心，普通容器为精密立方核心，离线时高度下沉 `0.14m`）</li>
                    <li>中段拉丝金属散热鳍片环（Heatsink Ring）</li>
                    <li>独立可抬升的容器光学玻璃腔体罩（选中该容器时自动抬升拆解）</li>
                    <li>顶部端点公告天线环（<strong>仅当 `endpoints.length &gt; 0` 时渲染显示</strong>，无端点容器顶部平坦无天线，几何级直观区分）</li>
                    <li>底部选中态校准光圈（Selection Calibration Halo）</li>
                  </ol>
                </li>
                <li>
                  <strong>连接轨道（Connection Tracks）</strong>：
                  点对点直连（Direct）采用青蓝色高对比实心抛物导管并附带双向数据脉冲球；中继/待定（Relay/Pending）采用琥珀色轨道；离线链路降为低透明度静态浅灰细轨且关闭脉冲；缓存快照模式下冻结所有流动球体。
                </li>
              </ul>
            </div>

            {/* 08. 五种核心状态规范与一键验证 */}
            <div className="bg-white border border-slate-200 rounded-lg p-5 space-y-3">
              <div className="text-xs font-mono text-slate-500">08. FIVE CANONICAL STATES</div>
              <h2 className="text-base font-bold text-slate-900">
                08. 正常、离线、缓存、无端点、失联五种状态规范（点击可直接跳转验证）
              </h2>
              <div className="space-y-2 text-xs">
                {[
                  {
                    key: 'normal' as ScenarioKey,
                    title: '状态 1：正常在线 (4 / 5 在线设备)',
                    desc: '锚点与主力容器点对点直连，完整涵盖直连、仅出站无端点与常规离线节点。',
                  },
                  {
                    key: 'partial_offline' as ScenarioKey,
                    title: '状态 2：节点离线 (3 / 5 在线 · 无红色恐慌)',
                    desc: '模拟推理容器训练完成后休眠离线：在线数减少，节点变中性灰保留在名册中，详情显示最后在线时间。',
                  },
                  {
                    key: 'no_endpoint' as ScenarioKey,
                    title: '状态 3：无端点成员与受控中继 (endpoints: [])',
                    desc: '设备已属于网络名册，但无公网入站端口或未开启 --listen，明确区分“仅出站待公告”与“经锚点中继”。',
                  },
                  {
                    key: 'discovery_lost' as ScenarioKey,
                    title: '状态 4 & 5：发现节点失联 & 缓存数据快照',
                    desc: '顶部变为琥珀色警告，保留最后一次有效快照 v42，明确标注“当前显示的是缓存数据”与自动重试进度，恢复后自动更新。',
                  },
                ].map((st) => (
                  <div
                    key={st.key}
                    className="p-3 border border-slate-200 rounded-md flex items-center justify-between gap-3 hover:border-sky-500 transition-colors"
                  >
                    <div>
                      <div className="font-semibold text-slate-900">{st.title}</div>
                      <div className="text-slate-600 mt-0.5">{st.desc}</div>
                    </div>
                    <button
                      onClick={() => onApplyScenarioAndJump(st.key)}
                      className="px-3 py-1.5 bg-sky-50 hover:bg-sky-100 text-sky-700 font-medium rounded shrink-0 cursor-pointer whitespace-nowrap"
                    >
                      在 3D 舱中验证 →
                    </button>
                  </div>
                ))}
              </div>
            </div>
          </div>

          {/* 09. 中文文案规范 (反误导对照表) */}
          <div className="bg-white border border-slate-200 rounded-lg p-5 space-y-3">
            <div className="text-xs font-mono text-slate-500">09. CHINESE COPYWRITING & TERMINOLOGY RULES</div>
            <h2 className="text-base font-bold text-slate-900">09. 中文文案与反误导术语规范（正误对照）</h2>
            <div className="grid grid-cols-1 md:grid-cols-2 gap-3 text-xs">
              {[
                {
                  wrong: '“设备 home-nas-vault 已离线并从网络移除 / 节点崩溃告警”',
                  right: '“○ 当前离线（仍在网络名册 v42 中 · 最后在线 14:35:10）”',
                  why: '离线不等于被删除，也不等于安全事故。',
                },
                {
                  wrong: '“ci-runner-ephemeral 已批准入网，其他设备现在可以直接拨通”',
                  right: '“◆ 已属于网络名册 · 当前无公告端点（可能未开启 --listen 或尚未完成首次公告）”',
                  why: '已批准入网不等于已经提供了可被其他节点拨号的地址。',
                },
                {
                  wrong: '“没有公告端点 = 未加入网络” 或 “有端点 = 一定在线”',
                  right: '“名册成员关系（Membership）、当前在线（Liveness）与公告端点（Announcement）三者独立显示”',
                  why: '无特权容器可能只出站不监听端口；离线节点也可能保留历史缓存端点。',
                },
                {
                  wrong: '发现节点断连时静默显示旧列表，或直接白屏报错要求用户按 F5 刷新',
                  right: '“▲ 发现节点失联 · 当前显示的是缓存快照（16:08:00）· 正在后台自动重连 (8s)”',
                  why: '面板还能看到数据不代表是实时数据；恢复后必须自动更新且无需刷新页面。',
                },
              ].map((item, idx) => (
                <div key={idx} className="p-3 border border-slate-200 rounded-md space-y-1.5 bg-slate-50/50">
                  <div className="flex items-start gap-1.5 text-rose-700 font-medium">
                    <XCircle className="w-4 h-4 shrink-0 mt-0.5" />
                    <span>禁止表达：{item.wrong}</span>
                  </div>
                  <div className="flex items-start gap-1.5 text-emerald-800 font-semibold">
                    <CheckCircle2 className="w-4 h-4 shrink-0 mt-0.5" />
                    <span>标准文案：{item.right}</span>
                  </div>
                  <div className="text-[11px] text-slate-500 pl-5">依据：{item.why}</div>
                </div>
              ))}
            </div>
          </div>
        </div>
      )}

      {/* ================= SECTIONS 10 - 12 ================= */}
      {showSec('sec10_12') && (
        <div className="grid grid-cols-1 lg:grid-cols-3 gap-6">
          {/* 10. 桌面端与移动端适配方案 */}
          <div className="bg-white border border-slate-200 rounded-lg p-5 space-y-3">
            <div className="flex items-center justify-between text-xs font-mono text-slate-500">
              <span>10. CROSS-PLATFORM ADAPTATION</span>
              <Smartphone className="w-4 h-4 text-sky-600" />
            </div>
            <h2 className="text-base font-bold text-slate-900">
              10. Windows / macOS 桌面端与移动端适配方案
            </h2>
            <ul className="space-y-2 text-xs text-slate-700 leading-relaxed">
              <li>
                <strong>Windows / macOS 桌面客户端（≥ 1024px）</strong>：采用左右分栏工程台布局——左侧为自适应全高 3D 嵌套实验室视口与底部折叠控制坞，右侧为 430px 固定宽三级嵌套检视抽屉。支持高分屏（Retina / HiDPI）与无 GPU 环境（Software WebGL 帧率自适应节流）。
              </li>
              <li>
                <strong>iOS / Android 移动端与窄屏容器（&lt; 1024px）</strong>：
                <ul className="list-disc list-inside pl-2 mt-1 space-y-1 text-slate-600">
                  <li>顶部粘性导航高度严格控制在视口高度 15% 以内；</li>
                  <li>首屏第一眼固定展示 `4 / 5 在线设备` 与缓存/同步横幅；</li>
                  <li>3D 视口收束为 `360px` 高度触控卡片，下方直接衔接机房与容器名册列表；</li>
                  <li>自动遵循系统 `prefers-reduced-motion` 减弱动态脉冲。</li>
                </ul>
              </li>
            </ul>
          </div>

          {/* 11. 后续一机多网和统一凭证的扩展策略 */}
          <div className="bg-white border border-slate-200 rounded-lg p-5 space-y-3">
            <div className="flex items-center justify-between text-xs font-mono text-slate-500">
              <span>11. MULTI-MESH & CREDENTIAL EXTENSIBILITY</span>
              <Shield className="w-4 h-4 text-sky-600" />
            </div>
            <h2 className="text-base font-bold text-slate-900">
              11. 后续「一机多网」与「统一凭证」扩展策略
            </h2>
            <p className="text-xs text-slate-600 leading-relaxed">
              在不把未来功能伪装成当前已上线能力的前提下，界面已在关键架构节点预留无损扩展槽位：
            </p>
            <div className="space-y-2 text-xs text-slate-700">
              <div className="p-2.5 bg-slate-50 border border-slate-200 rounded">
                <div className="font-semibold text-slate-900">1. 一机多网与多租网切换槽（顶栏左侧）</div>
                <p className="text-slate-600 mt-0.5">
                  当前显示单一网络 `prod-container-mesh [acct_9f82c104]`。后续支持一台设备同时挂载多个隔离网络（如 `corp-dev-mesh` + `home-lab-mesh`）时，可直接展开顶栏下拉抽屉切换对应名册与 3D 机房拓扑，无需重构主界面。
                </p>
              </div>
              <div className="p-2.5 bg-slate-50 border border-slate-200 rounded">
                <div className="font-semibold text-slate-900">2. 统一凭证生命周期槽（容器详情第 3 层）</div>
                <p className="text-slate-600 mt-0.5">
                  在容器详情的「四维状态卡」下方预留凭证状态栏，后续接入统一 Enrollment Token、到期倒计时提醒与一键吊销（Revoke）操作，并汇入审计记录抽屉。
                </p>
              </div>
            </div>
          </div>

          {/* 12. 高保真原型页面清单与产品事实边界 */}
          <div className="bg-white border border-slate-200 rounded-lg p-5 space-y-3">
            <div className="flex items-center justify-between text-xs font-mono text-slate-500">
              <span>12. PROTOTYPE PAGES & FACT BOUNDARIES</span>
              <Terminal className="w-4 h-4 text-sky-600" />
            </div>
            <h2 className="text-base font-bold text-slate-900">
              12. 高保真原型清单、Go `go:embed` 工程边界与事实边界
            </h2>
            <div className="space-y-2 text-xs">
              <div className="p-2.5 bg-emerald-50/70 border border-emerald-200 rounded">
                <div className="font-semibold text-emerald-950">当前已完成核心能力（真实可交互）：</div>
                <div className="text-emerald-900 mt-0.5">
                  用户态零特权组网 · 设备发现与 `/api/mesh` 接口 · 点对点消息通信 · 大文件传输 · RTT 延迟测量 · 白名单受控远程命令 · 多环境与慢链路验证。
                </div>
              </div>
              <div className="p-2.5 bg-amber-50/70 border border-amber-200 rounded">
                <div className="font-semibold text-amber-950">正在开发与后续产品化预留（明确标注不伪造）：</div>
                <div className="text-amber-900 mt-0.5">
                  可视化面板单文件嵌入（Go `go:embed` 零外部依赖构建） · 统一凭证与过期吊销 · 一键自动加入 · 一机多网与多租网共用锚点服务器。
                </div>
              </div>
              <div className="text-[11px] text-slate-500 pt-1">
                数据诚信铁律：绝不编造虚假吞吐量曲线、绝不编造综合网络健康评分、绝不把未连接的设备画成连通。
              </div>
            </div>
          </div>
        </div>
      )}
    </div>
  );
};
