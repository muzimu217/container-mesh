import React, { useEffect, useState, useCallback, useRef } from 'react';
import {
  MeshApiResponse,
  TransportMode,
} from './data/meshSnapshots';
import {
  fetchLiveRoster,
  panelToken,
  savePanelToken,
  LIVE_POLL_MS,
} from './data/liveMesh';
import { IsometricMeshCockpit } from './components/IsometricMeshCockpit';
import { NestedHierarchyInspector } from './components/NestedHierarchyInspector';
import {
  MinimalistProbeDashboard,
  ColorSchemePreference,
} from './components/MinimalistProbeDashboard';
import { DesignSystemManual } from './components/DesignSystemManual';

/**
 * The roster before the first answer arrives.
 *
 * Deliberately EMPTY rather than a demo network: the prototype's mock
 * scenarios showed a plausible-looking mesh for a moment before the real
 * data loaded, and a dashboard that can paint fiction is a dashboard that
 * lies.
 */
const EMPTY_ROSTER: MeshApiResponse = {
  account_id: '',
  network_name: '',
  roster_version: 0,
  generated_at: '',
  discovery_status: 'connected',
  is_cached: false,
  last_sync_at: '',
  retry_in_seconds: null,
  zones: [],
  members: [],
};
import {
  AlertTriangle,
  RefreshCw,
  CheckCircle2,
  SlidersHorizontal,
  Info,
  X,
  LayoutList,
  Globe,
  Sun,
  Moon,
  Monitor,
  Waves,
} from 'lucide-react';

export default function App() {
  const [activeWorkspace, setActiveWorkspace] = useState<
    'standard' | 'cockpit' | 'spec' | 'boundary'
  >('standard');  // The roster starts EMPTY: the cockpit shows the real devices as soon as
  // the first answer arrives, and never paints the prototype's demo
  // numbers in the meantime.
  const [meshData, setMeshData] = useState<MeshApiResponse>(EMPTY_ROSTER);
  const [needsToken, setNeedsToken] = useState<boolean>(false);
  const [liveError, setLiveError] = useState<string | null>(null);
  const [tokenInput, setTokenInput] = useState<string>('');
  const meshDataRef = useRef<MeshApiResponse>(EMPTY_ROSTER);
  useEffect(() => {
    meshDataRef.current = meshData;
  }, [meshData]);
  const [selectedZoneId, setSelectedZoneId] = useState<string | null>(null);
  const [selectedDeviceId, setSelectedDeviceId] = useState<string | null>(null);
  const [isFetchingApi, setIsFetchingApi] = useState<boolean>(false);
  const [retryCountdown, setRetryCountdown] = useState<number>(8);
  const [showFutureMultiMeshModal, setShowFutureMultiMeshModal] = useState<boolean>(false);
  const [activeCommEvent, setActiveCommEvent] = useState<{
    fromId: string;
    toId: string;
    label: string;
  } | null>(null);

  // Unified Glassmorphism Appearance Mode: 'light' | 'dark' | 'system'
  const [colorSchemePref, setColorSchemePref] = useState<ColorSchemePreference>('dark');
  // Global cmesh-orb Fluid Stream Animation Intensity (0% to 100%, default 75%)
  const [ambientIntensity, setAmbientIntensity] = useState<number>(75);
  const [systemPrefersDark, setSystemPrefersDark] = useState<boolean>(() => {
    if (typeof window !== 'undefined' && window.matchMedia) {
      return window.matchMedia('(prefers-color-scheme: dark)').matches;
    }
    return false;
  });

  useEffect(() => {
    if (typeof window === 'undefined' || !window.matchMedia) return;
    const mq = window.matchMedia('(prefers-color-scheme: dark)');
    const handler = (e: MediaQueryListEvent) => setSystemPrefersDark(e.matches);
    mq.addEventListener('change', handler);
    return () => mq.removeEventListener('change', handler);
  }, []);

  const resolvedDark =
    colorSchemePref === 'system' ? systemPrefersDark : colorSchemePref === 'dark';

  // Apply unified dark/light glass shell across both standard dashboard & 3D cockpit
  const isGlassDarkShell =
    (activeWorkspace === 'standard' || activeWorkspace === 'cockpit') && resolvedDark;

  // Live roster. The prototype fetched a mock endpoint and fell back to
  // canned scenarios; the product has one source of truth — the discovery
  // node — and a failed fetch keeps the last good roster behind the
  // "node unreachable" banner instead of painting demo data.
  const fetchMeshFromBackend = useCallback(async () => {
    setIsFetchingApi(true);
    const previous = meshDataRef.current;
    const { data, needsToken, error } = await fetchLiveRoster(previous);
    setMeshData(data);
    setNeedsToken(needsToken);
    setLiveError(error);
    if (data.retry_in_seconds) {
      setRetryCountdown(data.retry_in_seconds);
    }
    setIsFetchingApi(false);
  }, []);

  useEffect(() => {
    fetchMeshFromBackend();
    const timer = window.setInterval(() => {
      fetchMeshFromBackend();
    }, LIVE_POLL_MS);
    return () => window.clearInterval(timer);
  }, [fetchMeshFromBackend]);

  // Automatic retry countdown when the discovery node is unreachable.
  useEffect(() => {
    if (!meshData || meshData.discovery_status !== 'disconnected_cached') return;
    const timer = window.setInterval(() => {
      setRetryCountdown((prev) => (prev <= 1 ? Math.round(LIVE_POLL_MS / 1000) : prev - 1));
    }, 1000);
    return () => clearInterval(timer);
  }, [meshData.discovery_status]);

  const handleTriggerCommEvent = (fromId: string, toId: string, label: string) => {
    setActiveCommEvent({ fromId, toId, label });
    window.setTimeout(() => {
      setActiveCommEvent((current) => (current?.label === label ? null : current));
    }, 4500);
  };

  const onlineCount = meshData.members.filter((m) => m.online).length;
  const totalCount = meshData.members.length;
  const anchorNode = meshData.members.find((m) => m.is_anchor);

  const normalizedIntensity = Math.max(0, Math.min(1, ambientIntensity / 100));
  const speedMultiplier = Math.max(0.25, 0.4 + normalizedIntensity * 1.1);

  return (
    <div
      style={
        {
          '--cmesh-orb-intensity': normalizedIntensity,
          '--cmesh-orb-speed': speedMultiplier,
        } as React.CSSProperties
      }
      className={`min-h-screen flex flex-col transition-colors duration-500 relative overflow-x-hidden ${
        isGlassDarkShell
          ? 'bg-[#050912] text-slate-100'
          : 'bg-[#EEF4FA] text-slate-900'
      }`}
    >
      {/* Global Page-Level Fluid Stream & cmesh-orb Backdrop */}
      <div
        aria-hidden="true"
        className="pointer-events-none fixed inset-0 overflow-hidden z-0"
      >
        <div
          className={`absolute inset-0 transition-opacity duration-500 ${
            isGlassDarkShell ? 'cmesh-fluid-gradient-dark' : 'cmesh-fluid-gradient-light'
          }`}
          style={{
            opacity: Math.max(0.2, normalizedIntensity * 0.85),
            animationPlayState: ambientIntensity === 0 ? 'paused' : 'running',
          }}
        />
        <div
          className={`cmesh-orb cmesh-orb-a -top-36 -left-28 w-[520px] h-[520px] blur-3xl ${
            isGlassDarkShell ? 'bg-sky-500/20' : 'bg-sky-400/28'
          }`}
          style={{
            animationPlayState: ambientIntensity === 0 ? 'paused' : 'running',
          }}
        />
        <div
          className={`cmesh-orb cmesh-orb-b top-1/3 -right-32 w-[480px] h-[480px] blur-3xl ${
            isGlassDarkShell ? 'bg-emerald-500/15' : 'bg-teal-300/25'
          }`}
          style={{
            animationPlayState: ambientIntensity === 0 ? 'paused' : 'running',
          }}
        />
        <div
          className={`cmesh-orb cmesh-orb-c -bottom-36 left-1/3 w-[540px] h-[540px] blur-3xl ${
            isGlassDarkShell ? 'bg-indigo-500/18' : 'bg-indigo-300/24'
          }`}
          style={{
            animationPlayState: ambientIntensity === 0 ? 'paused' : 'running',
          }}
        />
      </div>

      {/* ================= 1. TOP BAR CONTRACT (Frosted Glass Header with Light/Dark/Auto Switcher) ================= */}
      <header
        className={`px-6 py-3 flex items-center justify-between gap-6 sticky top-0 z-30 border-b transition-colors duration-300 backdrop-blur-xl ${
          isGlassDarkShell
            ? 'bg-slate-950/75 border-white/[0.10] text-slate-100'
            : 'bg-white/80 border-white/90 shadow-2xs text-slate-900'
        }`}
      >
        {/* Zone 1: Brand Wordmark + Current Glass Mode Indicator */}
        <div className="flex items-center gap-3 shrink-0">
          <a
            href="#top"
            onClick={(e) => {
              e.preventDefault();
              setActiveWorkspace('standard');
              setSelectedZoneId(null);
              setSelectedDeviceId(null);
            }}
            className={`text-base font-bold tracking-tight font-mono whitespace-nowrap ${
              isGlassDarkShell ? 'text-white' : 'text-slate-900'
            }`}
          >
            cmesh
          </a>
          <span
            className={`text-[11px] font-mono hidden sm:inline ${
              isGlassDarkShell ? 'text-slate-400' : 'text-slate-500'
            }`}
          >
            · {resolvedDark ? '暗黑毛玻璃' : '亮色毛玻璃'}
            {colorSchemePref === 'system' ? ' (跟随系统)' : ''}
          </span>
        </div>

        {/* Zone 2: Concise Single-Line Navigation Links */}
        <nav
          className={`flex items-center gap-6 text-xs font-medium ${
            isGlassDarkShell ? 'text-slate-400' : 'text-slate-600'
          }`}
        >
          <button
            onClick={() => setActiveWorkspace('standard')}
            className={`py-1 border-b-2 transition-colors whitespace-nowrap shrink-0 cursor-pointer ${
              activeWorkspace === 'standard'
                ? isGlassDarkShell
                  ? 'border-sky-400 text-white font-semibold'
                  : 'border-sky-600 text-slate-900 font-semibold'
                : isGlassDarkShell
                ? 'border-transparent hover:text-white'
                : 'border-transparent hover:text-slate-900'
            }`}
          >
            普通模式 (实时流探针)
          </button>
          <button
            onClick={() => setActiveWorkspace('cockpit')}
            className={`py-1 border-b-2 transition-colors whitespace-nowrap shrink-0 cursor-pointer ${
              activeWorkspace === 'cockpit'
                ? isGlassDarkShell
                  ? 'border-sky-400 text-white font-semibold'
                  : 'border-sky-600 text-slate-900 font-semibold'
                : isGlassDarkShell
                ? 'border-transparent hover:text-white'
                : 'border-transparent hover:text-slate-900'
            }`}
          >
            3D 驾驶舱模式
          </button>
          <button
            onClick={() => setActiveWorkspace('spec')}
            className={`py-1 border-b-2 transition-colors whitespace-nowrap shrink-0 cursor-pointer ${
              activeWorkspace === 'spec'
                ? isGlassDarkShell
                  ? 'border-sky-400 text-white font-semibold'
                  : 'border-sky-600 text-slate-900 font-semibold'
                : isGlassDarkShell
                ? 'border-transparent hover:text-white'
                : 'border-transparent hover:text-slate-900'
            }`}
          >
            12 项设计系统规范
          </button>
          <button
            onClick={() => setActiveWorkspace('boundary')}
            className={`py-1 border-b-2 transition-colors whitespace-nowrap shrink-0 cursor-pointer ${
              activeWorkspace === 'boundary'
                ? isGlassDarkShell
                  ? 'border-sky-400 text-white font-semibold'
                  : 'border-sky-600 text-slate-900 font-semibold'
                : isGlassDarkShell
                ? 'border-transparent hover:text-white'
                : 'border-transparent hover:text-slate-900'
            }`}
          >
            接口契约与事实边界
          </button>
        </nav>

        {/* Zone 3: Global Fluid Animation Intensity + Appearance Switcher (Light / Dark / System Auto) + Sync Action */}
        <div className="flex items-center gap-2 shrink-0">
          {/* Global Fluid Stream Intensity Control */}
          <div
            className={`hidden md:flex items-center gap-1.5 px-2.5 py-1 rounded-lg border text-xs ${
              isGlassDarkShell
                ? 'bg-slate-900/80 border-white/10 text-slate-300'
                : 'bg-slate-100/90 border-slate-200/80 text-slate-600'
            }`}
            title="全局设置：调整 cmesh-orb 流动态流光背景动画强度 (0% 关闭 ~ 100% 极客增强)"
          >
            <Waves className="w-3.5 h-3.5 text-sky-400 shrink-0" />
            <span className="text-[11px] whitespace-nowrap">流光强度</span>
            <input
              type="range"
              min={0}
              max={100}
              step={5}
              value={ambientIntensity}
              onChange={(e) => setAmbientIntensity(parseInt(e.target.value, 10))}
              className="w-16 accent-sky-500 cursor-pointer"
            />
            <span className="font-mono text-[11px] tabular-nums w-8 text-right">
              {ambientIntensity}%
            </span>
          </div>

          <div
            className={`flex items-center gap-0.5 p-1 rounded-lg border text-xs ${
              isGlassDarkShell
                ? 'bg-slate-900/80 border-white/10 text-slate-300'
                : 'bg-slate-100/90 border-slate-200/80 text-slate-600'
            }`}
            title="右上角毛玻璃明暗模式切换（亮色 / 暗黑 / 自动跟随系统）"
          >
            <button
              onClick={() => setColorSchemePref('light')}
              className={`px-2 py-1 rounded flex items-center gap-1 transition-colors cursor-pointer ${
                colorSchemePref === 'light'
                  ? 'bg-white text-slate-900 shadow-2xs font-semibold'
                  : 'hover:opacity-100 opacity-75'
              }`}
              title="亮色毛玻璃模式"
            >
              <Sun className="w-3.5 h-3.5 text-amber-500" />
              <span className="hidden sm:inline">亮色</span>
            </button>
            <button
              onClick={() => setColorSchemePref('dark')}
              className={`px-2 py-1 rounded flex items-center gap-1 transition-colors cursor-pointer ${
                colorSchemePref === 'dark'
                  ? 'bg-slate-800 text-white shadow-2xs font-semibold'
                  : 'hover:opacity-100 opacity-75'
              }`}
              title="暗黑毛玻璃模式"
            >
              <Moon className="w-3.5 h-3.5 text-sky-400" />
              <span className="hidden sm:inline">暗黑</span>
            </button>
            <button
              onClick={() => setColorSchemePref('system')}
              className={`px-2 py-1 rounded flex items-center gap-1 transition-colors cursor-pointer ${
                colorSchemePref === 'system'
                  ? 'bg-sky-600 text-white shadow-2xs font-semibold'
                  : 'hover:opacity-100 opacity-75'
              }`}
              title="自动跟随系统明暗设置"
            >
              <Monitor className="w-3.5 h-3.5" />
              <span className="hidden sm:inline">自动</span>
            </button>
          </div>

          <button
            onClick={() => fetchMeshFromBackend()}
            className={`px-3 py-1.5 text-xs font-medium rounded-lg transition-colors flex items-center gap-1.5 whitespace-nowrap shrink-0 cursor-pointer ${
              isGlassDarkShell
                ? 'bg-sky-600 hover:bg-sky-500 text-white'
                : 'bg-slate-900 hover:bg-slate-800 text-white'
            }`}
          >
            <RefreshCw className={`w-3.5 h-3.5 ${isFetchingApi ? 'animate-spin' : ''}`} />
            <span>同步 /api/mesh</span>
          </button>
        </div>
      </header>

      {/* ================= 2. DESKTOP CLIENT TELEMETRY & DUAL-MODE / FIVE-STATE RIBBON ================= */}
      <section
        className={`border-b px-6 py-2.5 transition-colors duration-300 backdrop-blur-lg ${
          isGlassDarkShell
            ? 'bg-slate-900/55 border-white/[0.08] text-slate-200'
            : 'bg-white/75 border-white/80 text-slate-900'
        }`}
      >
        <div className="max-w-[1440px] mx-auto flex flex-col xl:flex-row xl:items-center justify-between gap-3">
          {/* Left: Mode 1 / Mode 2 Quick Switcher + Hero Readout ("4 / 5 在线设备") + Hover-Reveal Status Tokens */}
          <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
            {/* Dual Visualization Mode Switcher: Mode 1 (Minimalist Probe) <-> Mode 2 (3D Cockpit) */}
            <div
              className={`flex items-center gap-1 p-1 rounded-lg border ${
                isGlassDarkShell
                  ? 'bg-slate-950/70 border-white/10'
                  : 'bg-slate-100 border-slate-200/80'
              }`}
            >
              <button
                onClick={() => setActiveWorkspace('standard')}
                className={`flex items-center gap-1.5 px-2.5 py-1 rounded-md text-xs font-medium transition-colors cursor-pointer whitespace-nowrap ${
                  activeWorkspace === 'standard'
                    ? isGlassDarkShell
                      ? 'bg-sky-600 text-white shadow-2xs'
                      : 'bg-slate-900 text-white shadow-2xs'
                    : isGlassDarkShell
                    ? 'text-slate-400 hover:text-white'
                    : 'text-slate-600 hover:text-slate-900'
                }`}
                title="模式一：流动态毛玻璃组网探针台"
              >
                <LayoutList className="w-3.5 h-3.5" />
                <span>普通模式 · 实时流探针</span>
              </button>
              <button
                onClick={() => setActiveWorkspace('cockpit')}
                className={`flex items-center gap-1.5 px-2.5 py-1 rounded-md text-xs font-medium transition-colors cursor-pointer whitespace-nowrap ${
                  activeWorkspace === 'cockpit'
                    ? 'bg-sky-700 text-white shadow-2xs'
                    : isGlassDarkShell
                    ? 'text-slate-400 hover:text-white'
                    : 'text-slate-600 hover:text-slate-900'
                }`}
                title="模式二：3D 全息地球与等距沙盘驾驶舱"
              >
                <Globe className="w-3.5 h-3.5" />
                <span>3D 驾驶舱模式</span>
              </button>
            </div>

            {/* Hero Metric Readout */}
            <div
              className={`flex items-baseline gap-2 px-3 border-x ${
                isGlassDarkShell ? 'border-white/10' : 'border-slate-200'
              }`}
            >
              <span
                className={`text-xl font-bold font-mono tabular-nums ${
                  isGlassDarkShell ? 'text-white' : 'text-slate-900'
                }`}
              >
                {onlineCount} / {totalCount}
              </span>
              <span
                className={`text-xs font-semibold ${
                  isGlassDarkShell ? 'text-slate-300' : 'text-slate-700'
                }`}
              >
                在线设备
              </span>
            </div>

            {/* Compact Metadata with Hover-Only Detailed Explanations */}
            <div
              className={`flex flex-wrap items-center gap-x-3 gap-y-1 text-xs font-mono ${
                isGlassDarkShell ? 'text-slate-400' : 'text-slate-600'
              }`}
            >
              <div className="relative group cursor-help">
                <span
                  className={`font-semibold ${
                    isGlassDarkShell ? 'text-slate-100' : 'text-slate-900'
                  }`}
                >
                  {meshData.network_name}
                </span>
                <div className="pointer-events-none opacity-0 group-hover:opacity-100 transition-opacity duration-150 absolute left-0 top-full mt-2 w-64 p-2.5 bg-slate-900 text-white rounded-md shadow-lg z-40 font-sans">
                  <div className="font-semibold text-xs">当前私有网络身份</div>
                  <div className="text-[11px] text-slate-300 mt-1 font-mono">
                    Account ID: {meshData.account_id}
                  </div>
                  <div className="text-[11px] text-slate-300 mt-0.5">
                    用户态零特权管理模式（无需 root / CAP_NET_ADMIN）
                  </div>
                </div>
              </div>

              <span aria-hidden="true">·</span>

              <div className="relative group cursor-help">
                <span>v{meshData.roster_version}</span>
                <div className="pointer-events-none opacity-0 group-hover:opacity-100 transition-opacity duration-150 absolute left-0 top-full mt-2 w-60 p-2.5 bg-slate-900 text-white rounded-md shadow-lg z-40 font-sans">
                  <div className="font-semibold text-xs">名册版本号 (Roster Version)</div>
                  <div className="text-[11px] text-slate-300 mt-1">
                    快照时间: {meshData.generated_at.replace('T', ' ').slice(0, 19)} UTC
                  </div>
                </div>
              </div>

              <span aria-hidden="true">·</span>

              {/* Sync State Token (Hover for explanation) */}
              <div className="relative group cursor-help">
                <span
                  className={
                    meshData.is_cached
                      ? isGlassDarkShell
                        ? 'text-amber-400 font-semibold'
                        : 'text-amber-700 font-semibold'
                      : isGlassDarkShell
                      ? 'text-emerald-400 font-semibold'
                      : 'text-emerald-700 font-semibold'
                  }
                >
                  {meshData.is_cached ? '▲ 缓存视图' : '● 实时同步'}
                </span>
                <div className="pointer-events-none opacity-0 group-hover:opacity-100 transition-opacity duration-150 absolute left-0 top-full mt-2 w-72 p-2.5 bg-slate-900 text-white rounded-md shadow-lg z-40 font-sans">
                  <div className="font-semibold text-xs">
                    {meshData.is_cached
                      ? '发现节点连接中断 · 当前显示缓存快照'
                      : '发现节点连接正常 · 实时名册数据'}
                  </div>
                  <div className="text-[11px] text-slate-300 mt-1 leading-relaxed">
                    {meshData.is_cached
                      ? '面板当前展示的是最后一次有效快照，后台正自动重连发现节点，已建立的直连不受影响。'
                      : `已与自托管锚点 (${anchorNode?.display_name || 'anchor'}) 保持实时同步。`}
                  </div>
                </div>
              </div>

              <span aria-hidden="true">·</span>

              <button
                onClick={() => setShowFutureMultiMeshModal(true)}
                className={`hover:underline font-sans cursor-pointer ${
                  isGlassDarkShell ? 'text-sky-400' : 'text-sky-700'
                }`}
                title="查看后续一机多网与统一凭证扩展设计"
              >
                多网切换
              </button>
            </div>
          </div>

          {/* Right: live data source — the prototype's scenario switcher is gone.
              A production cockpit must not offer canned states: every number on
              screen comes from the discovery node's live roster. */}
          <div
            className={`flex flex-wrap items-center gap-1 p-1 rounded-lg border shrink-0 ${
              isGlassDarkShell
                ? 'bg-slate-950/70 border-white/10'
                : 'bg-slate-100 border-slate-200/80'
            }`}
          >
            <span
              className={`text-[11px] font-medium px-2 flex items-center gap-1 whitespace-nowrap ${
                isGlassDarkShell ? 'text-slate-400' : 'text-slate-500'
              }`}
            >
              <SlidersHorizontal className="w-3 h-3 text-sky-500" />
              <span>数据源:</span>
            </span>
            <span
              className={`text-[11px] font-mono px-2 whitespace-nowrap ${
                isGlassDarkShell ? 'text-slate-300' : 'text-slate-700'
              }`}
            >
              GET /api/mesh · 实时名册 v{meshData?.roster_version ?? 0}
            </span>
          </div>
        </div>
      </section>

      {/* ================= 3. DISCOVERY NODE LOST / CACHED SNAPSHOT WARNING BANNER ================= */}
      {meshData.is_cached && (        <div className="bg-amber-50 border-b border-amber-200 px-6 py-2.5 text-amber-950">
          <div className="max-w-[1440px] mx-auto flex flex-col sm:flex-row sm:items-center justify-between gap-2 text-xs">
            <div className="flex items-center gap-2">
              <AlertTriangle className="w-4 h-4 text-amber-600 shrink-0" />
              <span>
                <strong>发现节点连接中断 · 当前显示的是缓存数据</strong>：保留最后一次有效名册快照（v
                {meshData.roster_version}，生成于 {meshData.generated_at.slice(11, 19)} UTC）。已建立的点对点直连不受影响，后台正在自动重连（
                <span className="font-mono font-bold">{retryCountdown}s</span> 后自动恢复，无需手动刷新页面）。
              </span>
            </div>
            <button
              onClick={() => fetchMeshFromBackend()}
              className="px-3 py-1 bg-amber-700 hover:bg-amber-800 text-white rounded font-medium shrink-0 cursor-pointer whitespace-nowrap"
            >
              立即重试连接
            </button>
          </div>
        </div>
      )}

      {/* ================= TOKEN GATE =================
          The roster endpoint is protected by the panel token, so the cockpit
          asks for it once and keeps it in this browser. */}
      {needsToken && (
        <div className="fixed inset-0 z-[100] flex items-center justify-center bg-slate-950/80 backdrop-blur-sm">
          <div className="bg-white dark:bg-slate-900 rounded-xl border border-slate-200 dark:border-white/10 shadow-2xl p-6 max-w-md w-full mx-4">
            <div className="text-[11px] font-mono tracking-[0.12em] text-sky-600 dark:text-sky-400 mb-2">
              CMESH · PANEL
            </div>
            <div className="text-base font-bold text-slate-900 dark:text-slate-100 mb-1.5">
              需要面板令牌
            </div>
            <div className="text-xs text-slate-600 dark:text-slate-400 leading-relaxed mb-4">
              令牌在启动发现节点的终端里打印（<span className="font-mono">panel token</span>）。
              它只用于读取本网络的状态，请勿外传。
            </div>
            {liveError && (
              <div className="text-xs text-amber-700 dark:text-amber-300 mb-3">{liveError}</div>
            )}
            <div className="flex gap-2">
              <input
                type="password"
                autoComplete="off"
                value={tokenInput}
                onChange={(e) => setTokenInput(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') {
                    savePanelToken(tokenInput.trim());
                    fetchMeshFromBackend();
                  }
                }}
                placeholder="粘贴面板令牌"
                className="flex-1 min-w-0 rounded-lg border border-slate-300 dark:border-white/15 bg-slate-50 dark:bg-slate-950 px-3 py-2 font-mono text-xs text-slate-900 dark:text-slate-100"
              />
              <button
                onClick={() => {
                  savePanelToken(tokenInput.trim());
                  fetchMeshFromBackend();
                }}
                className="rounded-lg bg-sky-600 hover:bg-sky-700 text-white text-xs font-semibold px-4 py-2 cursor-pointer whitespace-nowrap"
              >
                连接
              </button>
            </div>
          </div>
        </div>
      )}

      {/* ================= 4. MAIN WORKSPACE CONTENT ================= */}
      <main className="relative z-10 flex-1 max-w-[1440px] w-full mx-auto px-6 py-5">
        {/* MODE 1: STANDARD MINIMALIST PROBE DASHBOARD (UNIFIED GLASSMORPHISM) */}
        {activeWorkspace === 'standard' && (
          <MinimalistProbeDashboard
            meshData={meshData}
            selectedZoneId={selectedZoneId}
            selectedDeviceId={selectedDeviceId}
            onSelectZone={(zId) => setSelectedZoneId(zId)}
            onSelectDevice={(dId) => setSelectedDeviceId(dId)}
            onSwitchTo3DCockpit={(zId, dId) => {
              if (zId !== undefined) setSelectedZoneId(zId);
              if (dId !== undefined) setSelectedDeviceId(dId);
              setActiveWorkspace('cockpit');
            }}
            colorSchemePref={colorSchemePref}
            resolvedDark={resolvedDark}
            onChangeColorSchemePref={setColorSchemePref}
            ambientIntensity={ambientIntensity}
            onChangeAmbientIntensity={setAmbientIntensity}
          />
        )}

        {/* MODE 2: 3D HOLOGRAPHIC EARTH & ISOMETRIC LAB COCKPIT */}
        {activeWorkspace === 'cockpit' && (
          <div className="flex flex-col lg:flex-row items-stretch gap-5 min-h-[640px]">
            <div className="flex-1 flex flex-col min-w-0">
              <IsometricMeshCockpit
                zones={meshData.zones}
                members={meshData.members}
                isCached={meshData.is_cached}
                selectedZoneId={selectedZoneId}
                selectedDeviceId={selectedDeviceId}
                onSelectZone={(zId) => setSelectedZoneId(zId)}
                onSelectDevice={(dId) => setSelectedDeviceId(dId)}
                activeCommEvent={activeCommEvent}
                ambientIntensity={ambientIntensity}
                onChangeAmbientIntensity={setAmbientIntensity}
                resolvedDark={resolvedDark}
              />
            </div>

            <NestedHierarchyInspector
              meshData={meshData}
              selectedZoneId={selectedZoneId}
              selectedDeviceId={selectedDeviceId}
              onSelectZone={(zId) => setSelectedZoneId(zId)}
              onSelectDevice={(dId) => setSelectedDeviceId(dId)}
            />
          </div>
        )}

        {activeWorkspace === 'spec' && (
          <DesignSystemManual
            onApplyScenarioAndJump={() => {
              setActiveWorkspace('standard');
            }}
          />
        )}

        {activeWorkspace === 'boundary' && (
          <div className="space-y-6 pb-10">
            <div className="bg-white border border-slate-200 rounded-lg p-6 space-y-4 text-slate-900">
              <div className="flex items-center justify-between">
                <div>
                  <div className="text-xs font-mono text-sky-700 font-semibold">
                    REAL API CONTRACT & PRODUCT FACT BOUNDARIES
                  </div>
                  <h2 className="text-xl font-bold text-slate-900 mt-1">
                    真实 `/api/mesh` 接口实时响应与产品事实边界验证
                  </h2>
                </div>
                <div className="flex items-center gap-2">
                  <button
                    onClick={() => setActiveWorkspace('standard')}
                    className="px-3.5 py-1.5 bg-slate-100 hover:bg-slate-200 text-slate-800 text-xs font-medium rounded cursor-pointer"
                  >
                    返回普通模式看板
                  </button>
                  <button
                    onClick={() => setActiveWorkspace('cockpit')}
                    className="px-3.5 py-1.5 bg-slate-900 text-white text-xs font-medium rounded cursor-pointer"
                  >
                    进入 3D 驾驶舱
                  </button>
                </div>
              </div>

              <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
                <div className="space-y-2">
                  <div className="flex items-center justify-between text-xs font-mono text-slate-600">
                    <span>GET /api/mesh (实时名册)</span>
                    <span className="text-emerald-700 font-semibold">200 OK · application/json</span>
                  </div>
                  <pre className="p-4 bg-slate-900 text-slate-100 rounded-md text-xs font-mono overflow-x-auto max-h-[460px] leading-relaxed">
                    {JSON.stringify(
                      {
                        account_id: meshData.account_id,
                        network_name: meshData.network_name,
                        roster_version: meshData.roster_version,
                        generated_at: meshData.generated_at,
                        discovery_status: meshData.discovery_status,
                        is_cached: meshData.is_cached,
                        members: meshData.members.map((m) => ({
                          device_id: m.device_id,
                          display_name: m.display_name,
                          online: m.online,
                          endpoints: m.endpoints,
                          last_seen: m.last_seen,
                          is_anchor: m.is_anchor,
                          transport: m.transport,
                        })),
                      },
                      null,
                      2
                    )}
                  </pre>
                </div>

                <div className="space-y-4 text-xs text-slate-700">
                  <div className="p-4 bg-slate-50 border border-slate-200 rounded-md space-y-2">
                    <div className="font-bold text-slate-900 text-sm flex items-center gap-1.5">
                      <CheckCircle2 className="w-4 h-4 text-emerald-600" />
                      <span>第一部分：当前已完成的核心事实能力</span>
                    </div>
                    <p className="text-slate-600 leading-relaxed">
                      以下能力已在 `cmesh` 核心架构中验证完成，并在本客户端容器详情检视器中提供真实交互闭环：
                    </p>
                    <ul className="space-y-1 font-mono text-[11px] text-slate-800 pl-2">
                      <li>· 用户态私有组网（不要求 root、不要求 CAP_NET_ADMIN、不依赖 TUN）</li>
                      <li>· 自托管锚点与设备名册发现（account_id / roster_version / endpoints）</li>
                      <li>· 容器间点对点加密消息发送（Userspace Message Stream）</li>
                      <li>· 大文件分片流式传输与 SHA-256 校验</li>
                      <li>· 真实按需 RTT 往返延迟探测（不编造后台常驻虚假曲线）</li>
                      <li>· 白名单受控远程命令执行（仅允许授权白名单指令）</li>
                    </ul>
                  </div>

                  <div className="p-4 bg-amber-50/60 border border-amber-200 rounded-md space-y-2">
                    <div className="font-bold text-amber-950 text-sm flex items-center gap-1.5">
                      <Info className="w-4 h-4 text-amber-600" />
                      <span>第二部分：后续产品化扩展能力（界面仅做预留，绝不伪造）</span>
                    </div>
                    <ul className="space-y-1 text-amber-900 pl-2">
                      <li>· <strong>统一凭证生命周期</strong>：一键入网凭证、过期时间与吊销管理（已预留槽位）</li>
                      <li>· <strong>一机多网与多租网切换</strong>：单客户端同时挂载多个隔离网络（见顶栏预留入口）</li>
                      <li>· <strong>单文件零外部依赖内嵌分发</strong>：通过 Go `go:embed` 将前端编译进单一二进制可执行文件</li>
                    </ul>
                  </div>
                </div>
              </div>
            </div>
          </div>
        )}
      </main>

      {/* ================= FUTURE MULTI-MESH EXTENSION PREVIEW MODAL ================= */}
      {showFutureMultiMeshModal && (
        <div className="fixed inset-0 z-50 bg-slate-900/40 flex items-center justify-center p-4">
          <div className="bg-white border border-slate-200 rounded-lg max-w-lg w-full p-5 shadow-lg space-y-4 text-slate-900">
            <div className="flex items-center justify-between border-b border-slate-200 pb-3">
              <div>
                <div className="text-[11px] font-mono text-sky-700 font-semibold">
                  FUTURE EXTENSION SLOT · MULTI-MESH & UNIFIED CREDENTIAL
                </div>
                <h3 className="text-base font-bold text-slate-900 mt-0.5">
                  多租网切换（一机多网）与统一凭证扩展预留说明
                </h3>
              </div>
              <button
                onClick={() => setShowFutureMultiMeshModal(false)}
                className="p-1 text-slate-400 hover:text-slate-700 cursor-pointer"
              >
                <X className="w-4 h-4" />
              </button>
            </div>

            <div className="space-y-3 text-xs text-slate-600 leading-relaxed">
              <p>
                根据产品设计边界规范（第十一章与第十五章），<strong>一机多网切换</strong>与<strong>统一凭证自动入网</strong>属于后续产品化阶段能力，当前版本明确标注为预留扩展槽位，不伪装成已上线功能：
              </p>
              <div className="p-3 bg-slate-50 border border-slate-200 rounded font-mono space-y-1.5">
                <div className="text-slate-900 font-semibold">
                  ● 当前激活网络: prod-container-mesh (acct_9f82c104 · Roster v{meshData.roster_version})
                </div>
                <div className="text-slate-400">
                  ○ [未来预留槽位] ai-agent-sandbox-net (支持同一客户端同时挂载多张私有网)
                </div>
                <div className="text-slate-400">
                  ○ [未来预留槽位] home-private-cloud (支持统一凭证一键加入与吊销管理)
                </div>
              </div>
            </div>

            <div className="flex justify-end pt-2">
              <button
                onClick={() => setShowFutureMultiMeshModal(false)}
                className="px-4 py-1.5 bg-slate-900 text-white text-xs font-medium rounded hover:bg-slate-800 cursor-pointer"
              >
                知悉并返回
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
