import React, { useState, useEffect } from 'react';
import { MeshApiResponse, MeshMember } from '../data/meshSnapshots';
import {
  Search,
  Globe,
  Activity,
  Send,
  FileUp,
  Terminal,
  Power,
  ChevronDown,
  ChevronUp,
  CheckCircle2,
  AlertTriangle,
  Sliders,
  ShieldCheck,
  Box,
  Layers,
  LayoutGrid,
  List,
  Sun,
  Moon,
  Monitor,
  ArrowUp,
  ArrowDown,
  Radio,
  Cpu,
  Gauge,
  Waves,
} from 'lucide-react';

export type ColorSchemePreference = 'light' | 'dark' | 'system';

interface MinimalistProbeDashboardProps {
  meshData: MeshApiResponse;
  selectedZoneId: string | null;
  selectedDeviceId: string | null;
  onSelectZone: (zoneId: string | null) => void;
  onSelectDevice: (deviceId: string | null) => void;
  onToggleDeviceOnline: (deviceId: string) => void;
  onTriggerCommEvent: (fromId: string, toId: string, label: string) => void;
  onSwitchTo3DCockpit: (zoneId?: string | null, deviceId?: string | null) => void;
  colorSchemePref?: ColorSchemePreference;
  resolvedDark?: boolean;
  onChangeColorSchemePref?: (pref: ColorSchemePreference) => void;
  ambientIntensity?: number;
  onChangeAmbientIntensity?: (val: number) => void;
}

type ProbeLayoutMode = 'grid' | 'list';

interface NodeRuntimeTelemetry {
  flag: string;
  regionCode: string;
  osDistro: string;
  kernelArch: string;
  uptimeStr: string;
  cpuCores: number;
  cpuPercent: number;
  memUsedGb: number;
  memTotalGb: number;
  diskUsedGb: number;
  diskTotalGb: number;
  netUpMbps: number;
  netDownMbps: number;
  trafficUpGb: number;
  trafficDownGb: number;
  load1: number;
  sparkline: number[];
  pingSeriesDirect: number[];
  pingSeriesAnchor: number[];
  pingSeriesRelay: number[];
  slaPercent: number;
  packetLossPercent: number;
}

const INITIAL_TELEMETRY: Record<string, NodeRuntimeTelemetry> = {
  dev_01_anchor_hk: {
    flag: '🇭🇰',
    regionCode: 'HKG',
    osDistro: 'Alpine 3.20',
    kernelArch: 'Linux 6.6 · x86_64',
    uptimeStr: '42天 18小时',
    cpuCores: 4,
    cpuPercent: 24,
    memUsedGb: 1.8,
    memTotalGb: 8.0,
    diskUsedGb: 28,
    diskTotalGb: 120,
    netUpMbps: 18.4,
    netDownMbps: 16.9,
    trafficUpGb: 842.6,
    trafficDownGb: 791.2,
    load1: 0.42,
    sparkline: [18, 22, 25, 21, 28, 32, 26, 23, 29, 25, 22, 24],
    pingSeriesDirect: [12, 13, 12, 14, 12, 13, 15, 12, 13, 12, 13, 12],
    pingSeriesAnchor: [1, 1, 1, 1, 2, 1, 1, 1, 1, 1, 1, 1],
    pingSeriesRelay: [18, 19, 18, 20, 19, 18, 21, 19, 18, 19, 18, 18],
    slaPercent: 99.98,
    packetLossPercent: 0.0,
  },
  dev_02_local_ws: {
    flag: '🇩🇪',
    regionCode: 'FRA',
    osDistro: 'macOS 15.2',
    kernelArch: 'Darwin · Apple M3 Max',
    uptimeStr: '14天 06小时',
    cpuCores: 14,
    cpuPercent: 31,
    memUsedGb: 19.4,
    memTotalGb: 36.0,
    diskUsedGb: 412,
    diskTotalGb: 1000,
    netUpMbps: 14.2,
    netDownMbps: 29.6,
    trafficUpGb: 312.4,
    trafficDownGb: 628.9,
    load1: 1.15,
    sparkline: [25, 28, 35, 30, 27, 33, 38, 31, 29, 34, 30, 31],
    pingSeriesDirect: [8, 9, 8, 8, 10, 9, 8, 9, 8, 8, 9, 8],
    pingSeriesAnchor: [136, 138, 135, 139, 137, 136, 141, 137, 136, 138, 136, 137],
    pingSeriesRelay: [152, 155, 151, 154, 153, 152, 158, 153, 151, 154, 152, 153],
    slaPercent: 99.95,
    packetLossPercent: 0.0,
  },
  dev_03_peer_direct: {
    flag: '🇨🇭',
    regionCode: 'ZRH',
    osDistro: 'Ubuntu 24.04',
    kernelArch: 'Linux 6.8 · CUDA 12.4',
    uptimeStr: '29天 11小时',
    cpuCores: 32,
    cpuPercent: 78,
    memUsedGb: 54.2,
    memTotalGb: 64.0,
    diskUsedGb: 1480,
    diskTotalGb: 2000,
    netUpMbps: 42.8,
    netDownMbps: 38.5,
    trafficUpGb: 2410.5,
    trafficDownGb: 1980.3,
    load1: 4.82,
    sparkline: [68, 74, 79, 82, 75, 84, 88, 79, 76, 81, 77, 78],
    pingSeriesDirect: [9, 9, 10, 9, 11, 9, 9, 10, 9, 9, 10, 9],
    pingSeriesAnchor: [142, 144, 141, 145, 143, 142, 146, 143, 142, 144, 143, 142],
    pingSeriesRelay: [161, 163, 160, 165, 162, 161, 166, 162, 160, 163, 161, 162],
    slaPercent: 99.92,
    packetLossPercent: 0.1,
  },
  dev_04_nat_container: {
    flag: '🇯🇵',
    regionCode: 'TYO',
    osDistro: 'Debian 12',
    kernelArch: 'containerd · Rootless',
    uptimeStr: '07天 22小时',
    cpuCores: 8,
    cpuPercent: 46,
    memUsedGb: 9.2,
    memTotalGb: 16.0,
    diskUsedGb: 64,
    diskTotalGb: 100,
    netUpMbps: 6.4,
    netDownMbps: 11.8,
    trafficUpGb: 128.7,
    trafficDownGb: 294.1,
    load1: 0.94,
    sparkline: [38, 42, 49, 44, 51, 47, 43, 45, 50, 48, 44, 46],
    pingSeriesDirect: [68, 72, 70, 74, 69, 71, 75, 70, 69, 72, 71, 70],
    pingSeriesAnchor: [48, 49, 47, 51, 49, 48, 52, 49, 48, 50, 49, 48],
    pingSeriesRelay: [54, 56, 53, 58, 55, 54, 59, 55, 54, 56, 55, 54],
    slaPercent: 99.4,
    packetLossPercent: 0.3,
  },
  dev_05_offline_nas: {
    flag: '🇸🇬',
    regionCode: 'SIN',
    osDistro: 'Debian 12 NAS',
    kernelArch: 'Podman · ARM64',
    uptimeStr: '离线 18分钟',
    cpuCores: 4,
    cpuPercent: 19,
    memUsedGb: 3.1,
    memTotalGb: 16.0,
    diskUsedGb: 6840,
    diskTotalGb: 8000,
    netUpMbps: 5.2,
    netDownMbps: 8.4,
    trafficUpGb: 512.0,
    trafficDownGb: 1120.8,
    load1: 0.28,
    sparkline: [16, 19, 21, 18, 22, 20, 17, 19, 23, 20, 18, 19],
    pingSeriesDirect: [34, 36, 35, 37, 35, 34, 38, 35, 34, 36, 35, 35],
    pingSeriesAnchor: [36, 37, 35, 38, 36, 36, 39, 37, 36, 37, 36, 36],
    pingSeriesRelay: [42, 44, 41, 45, 43, 42, 46, 43, 42, 44, 43, 42],
    slaPercent: 94.2,
    packetLossPercent: 1.2,
  },
};

export const MinimalistProbeDashboard: React.FC<MinimalistProbeDashboardProps> = ({
  meshData,
  selectedZoneId,
  selectedDeviceId,
  onSelectZone,
  onSelectDevice,
  onToggleDeviceOnline,
  onTriggerCommEvent,
  onSwitchTo3DCockpit,
  colorSchemePref = 'dark',
  resolvedDark = true,
  onChangeColorSchemePref,
  ambientIntensity = 75,
  onChangeAmbientIntensity,
}) => {
  const [layoutMode, setLayoutMode] = useState<ProbeLayoutMode>('grid');
  const [liveStreamEnabled, setLiveStreamEnabled] = useState<boolean>(true);
  const [telemetryMap, setTelemetryMap] =
    useState<Record<string, NodeRuntimeTelemetry>>(INITIAL_TELEMETRY);

  // Filters & collapsible sections
  const [searchQuery, setSearchQuery] = useState<string>('');
  const [statusFilter, setStatusFilter] = useState<'all' | 'online' | 'offline' | 'no_endpoint'>(
    'all'
  );
  const [showServiceMonitor, setShowServiceMonitor] = useState<boolean>(true);
  const [showTopologyStrip, setShowTopologyStrip] = useState<boolean>(false);
  const [showConfigAccordion, setShowConfigAccordion] = useState<boolean>(false);
  const [showDiagnosticsAccordion, setShowDiagnosticsAccordion] = useState<boolean>(false);
  const [drilldownTab, setDrilldownTab] = useState<'ping_chart' | 'load_chart'>('ping_chart');

  // Live probe action states
  const [actionLoading, setActionLoading] = useState<string | null>(null);
  const [actionOutput, setActionOutput] = useState<{
    deviceId: string;
    title: string;
    timestamp: string;
    detail: string;
  } | null>(null);
  const [msgInput, setMsgInput] = useState<string>('PING_PROBE: 验证容器用户态端到端连通');
  const [selectedCmd, setSelectedCmd] = useState<string>('cmesh status --json');

  // Smooth 1.5s real-time stream updates for online nodes
  useEffect(() => {
    if (!liveStreamEnabled) return;
    const timer = setInterval(() => {
      setTelemetryMap((prev) => {
        const next: Record<string, NodeRuntimeTelemetry> = { ...prev };
        meshData.members.forEach((member) => {
          const cur = prev[member.device_id] || INITIAL_TELEMETRY.dev_01_anchor_hk;
          if (!member.online) {
            next[member.device_id] = cur;
            return;
          }
          const deltaCpu = Math.round((Math.random() - 0.48) * 5);
          const nextCpu = Math.max(6, Math.min(95, cur.cpuPercent + deltaCpu));
          const deltaUp = (Math.random() - 0.48) * 2.4;
          const deltaDown = (Math.random() - 0.48) * 3.1;
          const nextUp = Math.max(0.4, Number((cur.netUpMbps + deltaUp).toFixed(1)));
          const nextDown = Math.max(0.6, Number((cur.netDownMbps + deltaDown).toFixed(1)));
          const nextSpark = [...cur.sparkline.slice(1), nextCpu];

          const basePing = cur.pingSeriesDirect[cur.pingSeriesDirect.length - 1] || 15;
          const nextPingDirect = Math.max(
            4,
            Math.round(basePing + (Math.random() - 0.5) * 3)
          );
          const baseAnchor = cur.pingSeriesAnchor[cur.pingSeriesAnchor.length - 1] || 30;
          const nextPingAnchor = Math.max(
            1,
            Math.round(baseAnchor + (Math.random() - 0.5) * 3)
          );
          const baseRelay = cur.pingSeriesRelay[cur.pingSeriesRelay.length - 1] || 45;
          const nextPingRelay = Math.max(
            8,
            Math.round(baseRelay + (Math.random() - 0.5) * 4)
          );

          next[member.device_id] = {
            ...cur,
            cpuPercent: nextCpu,
            netUpMbps: nextUp,
            netDownMbps: nextDown,
            trafficUpGb: Number((cur.trafficUpGb + nextUp * 0.0005).toFixed(2)),
            trafficDownGb: Number((cur.trafficDownGb + nextDown * 0.0005).toFixed(2)),
            sparkline: nextSpark,
            pingSeriesDirect: [...cur.pingSeriesDirect.slice(1), nextPingDirect],
            pingSeriesAnchor: [...cur.pingSeriesAnchor.slice(1), nextPingAnchor],
            pingSeriesRelay: [...cur.pingSeriesRelay.slice(1), nextPingRelay],
          };
        });
        return next;
      });
    }, 1500);
    return () => clearInterval(timer);
  }, [liveStreamEnabled, meshData.members]);

  const onlineMembers = meshData.members.filter((m) => m.online);
  const offlineMembers = meshData.members.filter((m) => !m.online);
  const directMembers = meshData.members.filter((m) => m.online && m.transport === 'direct');
  const noEndpointMembers = meshData.members.filter((m) => m.endpoints.length === 0);
  const anchorNode = meshData.members.find((m) => m.is_anchor);

  const selectedDevice = meshData.members.find((m) => m.device_id === selectedDeviceId) || null;

  // Aggregate global bandwidth & traffic across online nodes
  const totalUpMbps = onlineMembers.reduce(
    (acc, m) => acc + (telemetryMap[m.device_id]?.netUpMbps || 0),
    0
  );
  const totalDownMbps = onlineMembers.reduce(
    (acc, m) => acc + (telemetryMap[m.device_id]?.netDownMbps || 0),
    0
  );
  const totalTrafficUpGb = meshData.members.reduce(
    (acc, m) => acc + (telemetryMap[m.device_id]?.trafficUpGb || 0),
    0
  );
  const totalTrafficDownGb = meshData.members.reduce(
    (acc, m) => acc + (telemetryMap[m.device_id]?.trafficDownGb || 0),
    0
  );

  const filteredMembers = meshData.members.filter((m) => {
    if (selectedZoneId && m.zone_id !== selectedZoneId) return false;
    if (statusFilter === 'online' && !m.online) return false;
    if (statusFilter === 'offline' && m.online) return false;
    if (statusFilter === 'no_endpoint' && m.endpoints.length > 0) return false;

    if (searchQuery.trim()) {
      const q = searchQuery.toLowerCase();
      const matchName = m.display_name.toLowerCase().includes(q);
      const matchAlias = (m.alias || '').toLowerCase().includes(q);
      const matchCity = m.geo_city.toLowerCase().includes(q);
      const matchId = m.device_id.toLowerCase().includes(q);
      const matchEp = m.endpoints.some((ep) => ep.toLowerCase().includes(q));
      return matchName || matchAlias || matchCity || matchId || matchEp;
    }
    return true;
  });

  const runProbeAction = async (
    member: MeshMember,
    action: 'ping' | 'message' | 'file_transfer' | 'exec_whitelist'
  ) => {
    setActionLoading(`${member.device_id}:${action}`);
    try {
      const response = await fetch('/api/mesh/action', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          device_id: member.device_id,
          action,
          payload: {
            text: msgInput,
            command: selectedCmd,
            fileName: 'checkpoint-shard-04.safetensors',
            fileSize: '256.0 MB',
          },
        }),
      });
      const json = await response.json();

      if (action === 'ping') {
        onTriggerCommEvent(
          'dev_02_local_ws',
          member.device_id,
          `探针测速完成: macbook-studio-local ⇄ ${member.display_name} (${json.result.avg_rtt_ms} ms)`
        );
        setActionOutput({
          deviceId: member.device_id,
          title: `RTT 延迟探测结果 — ${member.display_name} (${member.geo_city})`,
          timestamp: json.timestamp,
          detail: `模式: ${json.result.mode} · 样本: [${json.result.samples_ms.join(' ms, ')} ms] · 平均 RTT: ${json.result.avg_rtt_ms} ms (丢包 ${json.result.packet_loss})`,
        });
      } else if (action === 'message') {
        onTriggerCommEvent(
          'dev_02_local_ws',
          member.device_id,
          `点对点消息已送达: -> ${member.display_name} [${json.result.msg_id}]`
        );
        setActionOutput({
          deviceId: member.device_id,
          title: `加密消息回执 (${json.result.encrypted}) — ${member.display_name}`,
          timestamp: json.timestamp,
          detail: `${json.result.echo} · 回执 ID: ${json.result.msg_id}`,
        });
      } else if (action === 'file_transfer') {
        onTriggerCommEvent(
          'dev_02_local_ws',
          member.device_id,
          `大文件分片校验完成: -> ${member.display_name} (256.0 MB)`
        );
        setActionOutput({
          deviceId: member.device_id,
          title: `大文件分片流式传输 — ${json.result.file_name}`,
          timestamp: json.timestamp,
          detail: `大小: ${json.result.file_size} · 校验: ${json.result.chunks_verified} · SHA-256: ${json.result.sha256.slice(0, 32)}...`,
        });
      } else if (action === 'exec_whitelist') {
        onTriggerCommEvent(
          'dev_02_local_ws',
          member.device_id,
          `受控白名单指令完成: ${selectedCmd} @ ${member.display_name}`
        );
        setActionOutput({
          deviceId: member.device_id,
          title: `受控白名单远程命令 (${json.result.command}) — ${member.display_name}`,
          timestamp: json.timestamp,
          detail: json.result.stdout,
        });
      }
    } catch {
      setActionOutput({
        deviceId: member.device_id,
        title: `本地探针回显 — ${member.display_name}`,
        timestamp: new Date().toISOString(),
        detail: `已通过用户态通道向 ${member.display_name} 发起探测请求。`,
      });
    } finally {
      setActionLoading(null);
    }
  };

  const isDark = resolvedDark;

  // Unified Edge-Lit Glassmorphism tokens (Light Glass vs Dark Glass)
  const wrapperClass = isDark
    ? 'bg-gradient-to-br from-[#060A12] via-[#0B1324] to-[#0E172E] text-slate-100 border border-white/[0.12] shadow-2xl'
    : 'bg-gradient-to-br from-[#E6EFF8] via-[#EDF4FB] to-[#E2ECF7] text-slate-900 border border-white/90 shadow-xl';

  const cardClass = isDark ? 'cmesh-glass-card-dark' : 'cmesh-glass-card-light';
  const subtleBgClass = isDark ? 'cmesh-glass-sub-dark' : 'cmesh-glass-sub-light';

  const textMutedClass = isDark ? 'text-slate-400' : 'text-slate-500';
  const textPrimaryClass = isDark ? 'text-slate-100' : 'text-slate-900';
  const dividerClass = isDark ? 'border-white/[0.09]' : 'border-slate-300/60';

  // Progress bar color feedback (<70% cyan/emerald, 70%-88% amber warning, >=88% rose alert)
  const getProgressBarColor = (pct: number, online: boolean) => {
    if (!online) return isDark ? 'bg-slate-700' : 'bg-slate-300';
    if (pct >= 88) return 'bg-rose-500';
    if (pct >= 70) return 'bg-amber-500';
    return 'bg-emerald-500';
  };

  const getDeviceStatusPresentation = (m: MeshMember) => {
    const hasEndpoints = m.endpoints.length > 0;
    if (!m.online) {
      return {
        glyph: '○',
        label: '离线 (在册)',
        dotClass: 'bg-slate-400',
        tone: isDark ? 'text-slate-400' : 'text-slate-500',
        transportText: '不可达 · 保留在名册',
      };
    }
    if (m.is_anchor) {
      return {
        glyph: '◈',
        label: '公网锚点',
        dotClass: 'bg-sky-400 shadow-[0_0_8px_rgba(56,189,248,0.85)]',
        tone: isDark ? 'text-sky-400' : 'text-sky-700',
        transportText: '自托管锚点 (Direct)',
      };
    }
    if (!hasEndpoints) {
      return {
        glyph: '◆',
        label: '无入站端点',
        dotClass: 'bg-amber-400 shadow-[0_0_8px_rgba(251,191,36,0.75)]',
        tone: isDark ? 'text-amber-400' : 'text-amber-700',
        transportText: '仅出站连接 (Outbound)',
      };
    }
    if (m.transport === 'relay') {
      return {
        glyph: '⇄',
        label: '锚点中继',
        dotClass: 'bg-amber-400 shadow-[0_0_8px_rgba(251,191,36,0.75)]',
        tone: isDark ? 'text-amber-400' : 'text-amber-700',
        transportText: '受控加密中继 (Relay)',
      };
    }
    return {
      glyph: '●',
      label: 'P2P 直连',
      dotClass: 'bg-emerald-400 shadow-[0_0_8px_rgba(52,211,153,0.85)]',
      tone: isDark ? 'text-emerald-400' : 'text-emerald-700',
      transportText: '点对点直连 (Direct P2P)',
    };
  };

  // Render lightweight SVG Sparkline
  const renderSparkline = (
    values: number[],
    online: boolean,
    accent: 'emerald' | 'sky' | 'amber'
  ) => {
    if (!online) {
      return (
        <svg viewBox="0 0 120 28" className="w-24 h-6 overflow-visible opacity-40">
          <line
            x1="0"
            y1="22"
            x2="120"
            y2="22"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeDasharray="3 3"
          />
        </svg>
      );
    }
    const max = Math.max(...values, 100);
    const min = 0;
    const pts = values
      .map((v, idx) => {
        const x = (idx / Math.max(1, values.length - 1)) * 120;
        const y = 25 - ((v - min) / (max - min)) * 21;
        return `${x.toFixed(1)},${y.toFixed(1)}`;
      })
      .join(' ');

    const strokeColor =
      accent === 'sky' ? '#0284c7' : accent === 'amber' ? '#d97706' : '#10b981';

    return (
      <svg viewBox="0 0 120 28" className="w-24 h-6 overflow-visible">
        <polyline
          fill="none"
          stroke={strokeColor}
          strokeWidth="1.85"
          strokeLinecap="round"
          strokeLinejoin="round"
          points={pts}
        />
      </svg>
    );
  };

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
      className={`rounded-2xl p-4 sm:p-5 space-y-5 transition-all duration-500 relative overflow-hidden ${wrapperClass}`}
    >
      {/* ================= FLUID DYNAMIC GLASSMORPHISM AMBIENT BACKGROUND ================= */}
      <div
        aria-hidden="true"
        className="pointer-events-none absolute inset-0 overflow-hidden z-0"
      >
        {/* Semi-transparent flowing gradient layer */}
        <div
          className={`absolute inset-0 transition-opacity duration-500 ${
            isDark ? 'cmesh-fluid-gradient-dark' : 'cmesh-fluid-gradient-light'
          }`}
          style={{
            opacity: Math.max(0.25, normalizedIntensity),
            animationPlayState: ambientIntensity === 0 ? 'paused' : 'running',
          }}
        />

        {/* Subtle Precision Coordinate Grid Overlay */}
        <div
          className="absolute inset-0 opacity-25"
          style={{
            backgroundImage: isDark
              ? 'radial-gradient(rgba(148, 163, 184, 0.22) 1px, transparent 1px)'
              : 'radial-gradient(rgba(71, 85, 105, 0.16) 1px, transparent 1px)',
            backgroundSize: '24px 24px',
          }}
        />

        {/* Fluid Orb A: Top-Left Cyan/Sky Stream */}
        <div
          className={`cmesh-orb cmesh-orb-a -top-28 -left-20 w-[440px] h-[440px] blur-3xl ${
            isDark ? 'bg-sky-500/28' : 'bg-sky-400/38'
          }`}
          style={{
            animationPlayState: ambientIntensity === 0 ? 'paused' : 'running',
          }}
        />

        {/* Fluid Orb B: Center-Right Emerald/Teal Stream */}
        <div
          className={`cmesh-orb cmesh-orb-b top-1/4 -right-24 w-[420px] h-[420px] blur-3xl ${
            isDark ? 'bg-emerald-500/22' : 'bg-teal-300/38'
          }`}
          style={{
            animationPlayState: ambientIntensity === 0 ? 'paused' : 'running',
          }}
        />

        {/* Fluid Orb C: Bottom-Center Indigo/Blue Stream */}
        <div
          className={`cmesh-orb cmesh-orb-c -bottom-32 left-1/3 w-[480px] h-[480px] blur-3xl ${
            isDark ? 'bg-indigo-500/25' : 'bg-indigo-300/34'
          }`}
          style={{
            animationPlayState: ambientIntensity === 0 ? 'paused' : 'running',
          }}
        />

        {/* Fluid Orb D: Center-Left Deep Geek Cyan Pulse */}
        <div
          className={`cmesh-orb cmesh-orb-d top-1/2 left-1/4 w-[360px] h-[360px] blur-3xl ${
            isDark ? 'bg-cyan-500/18' : 'bg-cyan-300/30'
          }`}
          style={{
            animationPlayState: ambientIntensity === 0 ? 'paused' : 'running',
          }}
        />
      </div>

      <div className="relative z-10 space-y-5">
        {/* ================= TOP CONTROL BAR: STREAM STATUS, VIEW LAYOUT & LIGHT/DARK/AUTO GLASS SWITCHER ================= */}
        <div
          className={`p-3.5 rounded-xl flex flex-wrap items-center justify-between gap-3 ${cardClass}`}
        >
          {/* Top-Left: Stream Title + Live Stream Toggle + Fluid Stream Intensity Slider */}
          <div className="flex flex-wrap items-center gap-2.5">
            <div className="flex items-center gap-2">
              <Radio
                className={`w-4 h-4 ${
                  liveStreamEnabled
                    ? isDark
                      ? 'text-emerald-400 animate-pulse'
                      : 'text-emerald-600 animate-pulse'
                    : textMutedClass
                }`}
              />
              <span className={`text-xs font-bold tracking-tight ${textPrimaryClass}`}>
                cmesh Pulse · 实时流组网探针台
              </span>
            </div>

            <span className={textMutedClass}>·</span>

            {/* Live Stream 1.5s Toggle */}
            <button
              onClick={() => setLiveStreamEnabled(!liveStreamEnabled)}
              className={`px-2.5 py-1 rounded-lg text-[11px] font-mono transition-colors cursor-pointer border ${
                liveStreamEnabled
                  ? isDark
                    ? 'bg-emerald-500/15 text-emerald-300 border-emerald-400/30'
                    : 'bg-emerald-500/15 text-emerald-800 border-emerald-500/30 font-semibold'
                  : `${subtleBgClass} ${textMutedClass}`
              }`}
              title="切换 1.5 秒零刷新流式数据推送"
            >
              {liveStreamEnabled ? '● 实时流推送 (1.5s)' : '○ 实时流已暂停'}
            </button>

            {/* Fluid Ambient Intensity Slider */}
            {onChangeAmbientIntensity && (
              <div
                className={`px-2.5 py-1 rounded-lg text-[11px] font-medium flex items-center gap-2 ${subtleBgClass}`}
                title="调整 cmesh-orb 流动态流光背景的动画强度 (0% - 100%)"
              >
                <Waves className="w-3.5 h-3.5 text-sky-500 shrink-0" />
                <span className={textMutedClass}>流光强度</span>
                <input
                  type="range"
                  min={0}
                  max={100}
                  step={5}
                  value={ambientIntensity}
                  onChange={(e) => onChangeAmbientIntensity(parseInt(e.target.value, 10))}
                  className="w-16 accent-sky-500 cursor-pointer"
                />
                <span className={`font-mono tabular-nums w-8 ${textPrimaryClass}`}>
                  {ambientIntensity}%
                </span>
              </div>
            )}
          </div>

          {/* Top-Right: Glassmorphism Light / Dark / System Auto Switcher + Grid/Stream List Switcher */}
          <div className="flex flex-wrap items-center gap-2">
            {/* Light / Dark / System Auto Mode Switcher */}
            {onChangeColorSchemePref && (
              <div
                className={`flex items-center gap-1 p-1 rounded-xl text-xs ${subtleBgClass}`}
                title="切换毛玻璃亮色模式、暗黑模式或自动跟随系统"
              >
                <button
                  onClick={() => onChangeColorSchemePref('light')}
                  className={`px-2.5 py-1 rounded-lg font-medium flex items-center gap-1.5 transition-all cursor-pointer ${
                    colorSchemePref === 'light'
                      ? 'bg-white text-slate-900 shadow-xs font-semibold'
                      : `${textMutedClass} hover:${textPrimaryClass}`
                  }`}
                >
                  <Sun className="w-3.5 h-3.5 text-amber-500" />
                  <span>亮色</span>
                </button>
                <button
                  onClick={() => onChangeColorSchemePref('dark')}
                  className={`px-2.5 py-1 rounded-lg font-medium flex items-center gap-1.5 transition-all cursor-pointer ${
                    colorSchemePref === 'dark'
                      ? 'bg-slate-900 text-white shadow-xs font-semibold'
                      : `${textMutedClass} hover:${textPrimaryClass}`
                  }`}
                >
                  <Moon className="w-3.5 h-3.5 text-sky-400" />
                  <span>暗黑</span>
                </button>
                <button
                  onClick={() => onChangeColorSchemePref('system')}
                  className={`px-2.5 py-1 rounded-lg font-medium flex items-center gap-1.5 transition-all cursor-pointer ${
                    colorSchemePref === 'system'
                      ? isDark
                        ? 'bg-sky-500/25 text-sky-200 border border-sky-400/30 font-semibold'
                        : 'bg-sky-600 text-white shadow-xs font-semibold'
                      : `${textMutedClass} hover:${textPrimaryClass}`
                  }`}
                >
                  <Monitor className="w-3.5 h-3.5" />
                  <span>跟随系统</span>
                </button>
              </div>
            )}

            {/* Grid Matrix vs Stream List Switcher */}
            <div className={`flex items-center gap-1 p-1 rounded-xl text-xs ${subtleBgClass}`}>
              <button
                onClick={() => setLayoutMode('grid')}
                className={`px-2.5 py-1 rounded-lg font-medium flex items-center gap-1 transition-all cursor-pointer ${
                  layoutMode === 'grid'
                    ? isDark
                      ? 'bg-slate-800/90 text-white shadow-xs font-semibold'
                      : 'bg-white text-slate-900 shadow-xs font-semibold'
                    : textMutedClass
                }`}
              >
                <LayoutGrid className="w-3.5 h-3.5" />
                <span>卡片矩阵</span>
              </button>
              <button
                onClick={() => setLayoutMode('list')}
                className={`px-2.5 py-1 rounded-lg font-medium flex items-center gap-1 transition-all cursor-pointer ${
                  layoutMode === 'list'
                    ? isDark
                      ? 'bg-slate-800/90 text-white shadow-xs font-semibold'
                      : 'bg-white text-slate-900 shadow-xs font-semibold'
                    : textMutedClass
                }`}
              >
                <List className="w-3.5 h-3.5" />
                <span>流式列表</span>
              </button>
            </div>
          </div>
        </div>

        {/* ================= 1. BENTO HERO STATS (顶部全局概览区 — 4 大毛玻璃指标卡) ================= */}
        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-3.5">
          {/* Hero Stat 1: Total & Online Nodes */}
          <div className={`p-4 rounded-xl flex flex-col justify-between ${cardClass}`}>
            <div className="flex items-center justify-between text-xs">
              <span className={textMutedClass}>在线节点 / 名册总数</span>
              <span
                className={`font-mono text-[11px] font-semibold tabular-nums ${
                  isDark ? 'text-emerald-400' : 'text-emerald-700'
                }`}
              >
                ● {Math.round((onlineMembers.length / meshData.members.length) * 100)}% 在线率
              </span>
            </div>
            <div className="mt-2.5 flex items-baseline justify-between">
              <div className="flex items-baseline gap-1.5 font-mono tabular-nums">
                <span className={`text-3xl font-bold ${textPrimaryClass}`}>
                  {onlineMembers.length}
                </span>
                <span className={`text-base ${textMutedClass}`}>
                  / {meshData.members.length} 台
                </span>
              </div>
              <div className="text-xs font-mono tabular-nums flex items-center gap-2">
                <span className={isDark ? 'text-emerald-400' : 'text-emerald-700 font-semibold'}>
                  {onlineMembers.length} 在线
                </span>
                <span className={textMutedClass}>·</span>
                <span className={textMutedClass}>{offlineMembers.length} 离线</span>
              </div>
            </div>
            <div className="mt-3 h-1.5 w-full rounded-full overflow-hidden flex bg-slate-500/20">
              <div
                className="bg-emerald-500 transition-all duration-500"
                style={{ width: `${(onlineMembers.length / meshData.members.length) * 100}%` }}
              />
            </div>
          </div>

          {/* Hero Stat 2: Real-Time Up/Down Bandwidth */}
          <div className={`p-4 rounded-xl flex flex-col justify-between ${cardClass}`}>
            <div className="flex items-center justify-between text-xs">
              <span className={textMutedClass}>全网实时上下行速率</span>
              <span className={`font-mono text-[11px] ${textMutedClass}`}>用户态加密流</span>
            </div>
            <div className="mt-2 grid grid-cols-2 gap-2 font-mono tabular-nums">
              <div>
                <div
                  className={`text-[11px] flex items-center gap-0.5 ${
                    isDark ? 'text-sky-400' : 'text-sky-700 font-medium'
                  }`}
                >
                  <ArrowUp className="w-3 h-3" />
                  <span>出站上传</span>
                </div>
                <div className={`text-xl font-bold mt-0.5 ${textPrimaryClass}`}>
                  {totalUpMbps.toFixed(1)}{' '}
                  <span className={`text-xs font-normal ${textMutedClass}`}>MB/s</span>
                </div>
              </div>
              <div>
                <div
                  className={`text-[11px] flex items-center gap-0.5 ${
                    isDark ? 'text-emerald-400' : 'text-emerald-700 font-medium'
                  }`}
                >
                  <ArrowDown className="w-3 h-3" />
                  <span>入站下载</span>
                </div>
                <div className={`text-xl font-bold mt-0.5 ${textPrimaryClass}`}>
                  {totalDownMbps.toFixed(1)}{' '}
                  <span className={`text-xs font-normal ${textMutedClass}`}>MB/s</span>
                </div>
              </div>
            </div>
            <div className={`mt-2 text-[11px] font-mono tabular-nums ${textMutedClass}`}>
              零 root / 零 TUN · 纯用户态直连与中继聚合
            </div>
          </div>

          {/* Hero Stat 3: Cumulative Monthly Traffic & P2P Ratio */}
          <div className={`p-4 rounded-xl flex flex-col justify-between ${cardClass}`}>
            <div className="flex items-center justify-between text-xs">
              <span className={textMutedClass}>累计总流量 & 通道结构</span>
              <span
                className={`font-mono text-[11px] font-semibold tabular-nums ${
                  isDark ? 'text-sky-400' : 'text-sky-700'
                }`}
              >
                {directMembers.length} 直连 / {noEndpointMembers.length} 无端点
              </span>
            </div>
            <div className="mt-2 flex items-baseline gap-2 font-mono tabular-nums">
              <span className={`text-2xl font-bold ${textPrimaryClass}`}>
                {((totalTrafficUpGb + totalTrafficDownGb) / 1024).toFixed(2)} TB
              </span>
              <span className={`text-xs ${textMutedClass}`}>周期累计吞吐</span>
            </div>
            <div
              className={`mt-2 text-[11px] font-mono tabular-nums flex items-center justify-between ${textMutedClass}`}
            >
              <span>↑ 出站 {(totalTrafficUpGb / 1024).toFixed(2)} TB</span>
              <span>↓ 入站 {(totalTrafficDownGb / 1024).toFixed(2)} TB</span>
            </div>
          </div>

          {/* Hero Stat 4: Self-Hosted Anchor & 3D Cockpit Jump */}
          <div className={`p-4 rounded-xl flex flex-col justify-between ${cardClass}`}>
            <div className="flex items-center justify-between text-xs">
              <span className={textMutedClass}>自托管信任锚点</span>
              <span
                className={`font-mono text-[11px] font-semibold ${
                  meshData.is_cached
                    ? isDark
                      ? 'text-amber-400'
                      : 'text-amber-700'
                    : isDark
                    ? 'text-emerald-400'
                    : 'text-emerald-700'
                }`}
              >
                {meshData.is_cached ? '▲ 缓存快照' : '● 实时同步'}
              </span>
            </div>
            <div className={`mt-2 font-mono text-sm font-bold truncate ${textPrimaryClass}`}>
              ◈ {anchorNode?.display_name || 'anchor-hk-01'} ({anchorNode?.geo_city.split(' ')[0]})
            </div>
            <div className="mt-2.5 flex items-center justify-between gap-2">
              <span className={`text-[11px] font-mono tabular-nums truncate ${textMutedClass}`}>
                {anchorNode?.endpoints[0] || '203.0.113.10:41820'}
              </span>
              <button
                onClick={() => onSwitchTo3DCockpit(selectedZoneId, selectedDeviceId)}
                className="px-2.5 py-1 bg-sky-600 hover:bg-sky-500 text-white rounded-lg text-xs font-medium flex items-center gap-1 transition-colors cursor-pointer shrink-0 shadow-xs"
              >
                <Globe className="w-3.5 h-3.5" />
                <span>3D 全息地球</span>
              </button>
            </div>
          </div>
        </div>

        {/* ================= FILTER & ZONE GROUPING BAR ================= */}
        <div
          className={`p-3 rounded-xl flex flex-col lg:flex-row lg:items-center justify-between gap-3 ${cardClass}`}
        >
          {/* Left: Zone Group Tabs */}
          <div className="flex flex-wrap items-center gap-1.5 text-xs">
            <button
              onClick={() => {
                onSelectZone(null);
                onSelectDevice(null);
              }}
              className={`px-3 py-1.5 rounded-lg font-medium transition-colors cursor-pointer ${
                !selectedZoneId
                  ? 'bg-sky-600 text-white shadow-xs'
                  : `${textMutedClass} hover:${textPrimaryClass}`
              }`}
            >
              全部区域 ({meshData.members.length})
            </button>
            {meshData.zones.map((z) => {
              const zCount = meshData.members.filter((m) => m.zone_id === z.zone_id).length;
              const isAct = selectedZoneId === z.zone_id;
              return (
                <button
                  key={z.zone_id}
                  onClick={() => {
                    onSelectZone(isAct ? null : z.zone_id);
                    onSelectDevice(null);
                  }}
                  className={`px-3 py-1.5 rounded-lg font-medium transition-colors cursor-pointer flex items-center gap-1.5 ${
                    isAct
                      ? 'bg-sky-600 text-white shadow-xs'
                      : `${textMutedClass} hover:${textPrimaryClass}`
                  }`}
                >
                  <span>{z.zone_name}</span>
                  <span className="font-mono text-[11px] opacity-75">({zCount})</span>
                </button>
              );
            })}
          </div>

          {/* Right: Status Filter + Search + 2D Topology Toggle */}
          <div className="flex flex-wrap items-center gap-2">
            <div className="relative">
              <Search
                className={`w-3.5 h-3.5 absolute left-2.5 top-1/2 -translate-y-1/2 ${textMutedClass}`}
              />
              <input
                type="text"
                value={searchQuery}
                onChange={(e) => setSearchQuery(e.target.value)}
                placeholder="搜索节点 / 地区 / IP..."
                className={`pl-8 pr-3 py-1 text-xs rounded-lg border font-mono focus:outline-none focus:border-sky-500 w-44 ${
                  isDark
                    ? 'bg-slate-900/60 border-white/15 text-slate-100 placeholder:text-slate-400'
                    : 'bg-white/75 border-white/90 text-slate-900 placeholder:text-slate-400'
                }`}
              />
            </div>

            <div className={`flex items-center gap-1 p-1 rounded-lg text-xs ${subtleBgClass}`}>
              {(
                [
                  { key: 'all', label: '全部' },
                  { key: 'online', label: '在线' },
                  { key: 'offline', label: '离线' },
                  { key: 'no_endpoint', label: '无端点' },
                ] as const
              ).map((tab) => (
                <button
                  key={tab.key}
                  onClick={() => setStatusFilter(tab.key)}
                  className={`px-2 py-0.5 rounded-md font-medium transition-colors cursor-pointer ${
                    statusFilter === tab.key
                      ? isDark
                        ? 'bg-slate-800 text-white shadow-2xs'
                        : 'bg-white text-slate-900 shadow-2xs font-semibold'
                      : textMutedClass
                  }`}
                >
                  {tab.label}
                </button>
              ))}
            </div>

            <button
              onClick={() => setShowTopologyStrip(!showTopologyStrip)}
              className={`px-2.5 py-1 rounded-lg text-xs font-medium transition-colors cursor-pointer ${subtleBgClass} ${textMutedClass} hover:${textPrimaryClass}`}
            >
              {showTopologyStrip ? '收起拓扑简图' : '2D 拓扑简图'}
            </button>
          </div>
        </div>

        {/* Optional Collapsible 2D Schematic Topology Strip */}
        {showTopologyStrip && (
          <div className={`p-4 rounded-xl ${cardClass}`}>
            <div
              className={`flex flex-wrap items-center justify-between gap-2 pb-3 border-b text-xs ${dividerClass}`}
            >
              <div className="flex items-center gap-2">
                <Layers className="w-4 h-4 text-sky-500" />
                <span className={`font-semibold ${textPrimaryClass}`}>
                  2D 组网连通关系简图（点击任意节点可展开右侧下钻详情）
                </span>
              </div>
              <div
                className={`flex flex-wrap items-center gap-4 text-[11px] font-mono ${textMutedClass}`}
              >
                <span>─── 实线: Direct P2P 直连</span>
                <span>- - - 虚线: 仅出站协调 / 锚点中继</span>
              </div>
            </div>
            <div className="mt-3 grid grid-cols-1 md:grid-cols-5 gap-2.5">
              {meshData.members.map((m) => {
                const st = getDeviceStatusPresentation(m);
                const t = telemetryMap[m.device_id] || INITIAL_TELEMETRY.dev_01_anchor_hk;
                const isSel = selectedDeviceId === m.device_id;
                return (
                  <button
                    key={m.device_id}
                    onClick={() => {
                      onSelectZone(m.zone_id);
                      onSelectDevice(isSel ? null : m.device_id);
                    }}
                    className={`p-3 rounded-lg text-left transition-all cursor-pointer ${
                      isSel
                        ? 'border border-sky-500 ring-2 ring-sky-500/30 bg-sky-500/10'
                        : `${subtleBgClass} hover:border-sky-500/40`
                    }`}
                  >
                    <div className="flex items-center justify-between text-xs font-mono">
                      <span className={`font-bold truncate ${textPrimaryClass}`}>
                        {t.flag} {m.display_name}
                      </span>
                      <span className={st.tone}>{st.glyph}</span>
                    </div>
                    <div className={`text-[11px] font-mono mt-1 truncate ${textMutedClass}`}>
                      {m.endpoints[0] || 'endpoints: [] (仅出站)'}
                    </div>
                  </button>
                );
              })}
            </div>
          </div>
        )}

        {/* ================= 2. CORE NODE MATRIX (GRID) / STREAM LIST + DRILL-DOWN DRAWER ================= */}
        <div className="flex flex-col xl:flex-row items-start gap-5">
          {/* Left / Main Area: Grid Matrix or Stream List */}
          <div className="flex-1 w-full space-y-4">
            {layoutMode === 'grid' ? (
              /* ---------- EDGE-LIT GLASSMORPHISM BENTO CARDS (GRID MATRIX) ---------- */
              <div className="grid grid-cols-1 md:grid-cols-2 2xl:grid-cols-3 gap-4">
                {filteredMembers.map((member) => {
                  const st = getDeviceStatusPresentation(member);
                  const t =
                    telemetryMap[member.device_id] || INITIAL_TELEMETRY.dev_01_anchor_hk;
                  const isSelected = selectedDeviceId === member.device_id;
                  const memPct = member.online
                    ? Math.round((t.memUsedGb / t.memTotalGb) * 100)
                    : 0;
                  const diskPct = Math.round((t.diskUsedGb / t.diskTotalGb) * 100);
                  const cpuPct = member.online ? t.cpuPercent : 0;
                  const isPinging = actionLoading === `${member.device_id}:ping`;
                  const latestRtt =
                    t.pingSeriesDirect[t.pingSeriesDirect.length - 1] || 15;

                  return (
                    <div
                      key={member.device_id}
                      onClick={() => {
                        onSelectZone(member.zone_id);
                        onSelectDevice(isSelected ? null : member.device_id);
                      }}
                      className={`rounded-xl p-4 transition-all duration-200 cursor-pointer flex flex-col justify-between ${cardClass} ${
                        isSelected
                          ? 'ring-2 ring-sky-500'
                          : !member.online
                          ? 'opacity-75 hover:opacity-100'
                          : 'hover:-translate-y-0.5'
                      }`}
                    >
                      {/* 1. Card Header: Flag + Node Name + Distro | Breathing Status Dot + Uptime */}
                      <div>
                        <div className="flex items-start justify-between gap-2">
                          <div className="min-w-0">
                            <div className="flex items-center gap-2">
                              <span className="text-base leading-none" aria-hidden="true">
                                {t.flag}
                              </span>
                              <span
                                className={`font-mono text-sm font-bold truncate ${textPrimaryClass}`}
                              >
                                {member.display_name}
                              </span>
                              {member.is_anchor && (
                                <span
                                  className={`text-[10px] font-mono font-semibold ${
                                    isDark ? 'text-sky-400' : 'text-sky-700'
                                  }`}
                                >
                                  [ANCHOR]
                                </span>
                              )}
                              {member.is_local && (
                                <span
                                  className={`text-[10px] font-mono font-semibold ${
                                    isDark ? 'text-emerald-400' : 'text-emerald-700'
                                  }`}
                                >
                                  [本机]
                                </span>
                              )}
                            </div>
                            <div
                              className={`text-[11px] font-mono mt-1 flex items-center gap-1.5 truncate ${textMutedClass}`}
                            >
                              <span>{t.osDistro}</span>
                              <span>·</span>
                              <span>{member.geo_city.split(' ')[0]}</span>
                              <span>·</span>
                              <span>{member.alias}</span>
                            </div>
                          </div>

                          {/* Right: Breathing Status Indicator + Uptime */}
                          <div className="text-right shrink-0">
                            <div className="inline-flex items-center gap-1.5 font-mono text-xs font-semibold">
                              <span className={`w-2 h-2 rounded-full ${st.dotClass}`} />
                              <span className={st.tone}>
                                {st.glyph} {st.label}
                              </span>
                            </div>
                            <div
                              className={`text-[11px] font-mono tabular-nums mt-0.5 ${textMutedClass}`}
                            >
                              {member.online ? `在线 ${t.uptimeStr}` : t.uptimeStr}
                            </div>
                          </div>
                        </div>

                        {/* 2. Core Resource Progress Bars (CPU / RAM / Disk) */}
                        <div className="mt-4 space-y-2.5">
                          {/* CPU Bar */}
                          <div>
                            <div className="flex items-center justify-between text-[11px] font-mono tabular-nums mb-1">
                              <span className={`flex items-center gap-1 ${textMutedClass}`}>
                                <Cpu className="w-3 h-3" />
                                <span>CPU ({t.cpuCores}C)</span>
                              </span>
                              <span
                                className={
                                  cpuPct >= 85
                                    ? 'text-rose-500 font-bold'
                                    : cpuPct >= 70
                                    ? 'text-amber-500 font-semibold'
                                    : textPrimaryClass
                                }
                              >
                                {member.online ? `${cpuPct}%` : '—'}
                              </span>
                            </div>
                            <div className="h-1.5 w-full rounded-full bg-slate-500/20 overflow-hidden">
                              <div
                                className={`h-full rounded-full transition-all duration-500 ${getProgressBarColor(
                                  cpuPct,
                                  member.online
                                )}`}
                                style={{ width: `${cpuPct}%` }}
                              />
                            </div>
                          </div>

                          {/* RAM & Disk Side-by-Side */}
                          <div className="grid grid-cols-2 gap-3">
                            <div>
                              <div className="flex items-center justify-between text-[11px] font-mono tabular-nums mb-1">
                                <span className={textMutedClass}>RAM</span>
                                <span className={textPrimaryClass}>
                                  {member.online
                                    ? `${t.memUsedGb.toFixed(1)}/${t.memTotalGb}G`
                                    : '—'}
                                </span>
                              </div>
                              <div className="h-1.5 w-full rounded-full bg-slate-500/20 overflow-hidden">
                                <div
                                  className={`h-full rounded-full transition-all duration-500 ${getProgressBarColor(
                                    memPct,
                                    member.online
                                  )}`}
                                  style={{ width: `${memPct}%` }}
                                />
                              </div>
                            </div>

                            <div>
                              <div className="flex items-center justify-between text-[11px] font-mono tabular-nums mb-1">
                                <span className={textMutedClass}>DISK</span>
                                <span className={textPrimaryClass}>{diskPct}%</span>
                              </div>
                              <div className="h-1.5 w-full rounded-full bg-slate-500/20 overflow-hidden">
                                <div
                                  className={`h-full rounded-full transition-all duration-500 ${getProgressBarColor(
                                    diskPct,
                                    true
                                  )}`}
                                  style={{ width: `${diskPct}%` }}
                                />
                              </div>
                            </div>
                          </div>
                        </div>

                        {/* 3. Real-time Network Speed (Tabular Nums) + Lightweight Sparkline */}
                        <div
                          className={`mt-3.5 pt-3 border-t flex items-center justify-between gap-2 ${dividerClass}`}
                        >
                          <div className="space-y-0.5 font-mono tabular-nums text-xs">
                            <div className="flex items-center gap-3">
                              <span
                                className={`font-semibold ${
                                  isDark ? 'text-sky-400' : 'text-sky-700'
                                }`}
                              >
                                ↑ {member.online ? `${t.netUpMbps.toFixed(1)} MB/s` : '0.0 MB/s'}
                              </span>
                              <span
                                className={`font-semibold ${
                                  isDark ? 'text-emerald-400' : 'text-emerald-700'
                                }`}
                              >
                                ↓ {member.online ? `${t.netDownMbps.toFixed(1)} MB/s` : '0.0 MB/s'}
                              </span>
                            </div>
                            <div className={`text-[11px] ${textMutedClass}`}>
                              累计 ↑{t.trafficUpGb.toFixed(0)}G / ↓{t.trafficDownGb.toFixed(0)}G
                            </div>
                          </div>

                          {/* Embedded SVG Sparkline */}
                          <div className="flex flex-col items-end">
                            {renderSparkline(
                              t.sparkline,
                              member.online,
                              member.is_anchor
                                ? 'sky'
                                : member.endpoints.length === 0
                                ? 'amber'
                                : 'emerald'
                            )}
                            <span className={`text-[10px] font-mono tabular-nums ${textMutedClass}`}>
                              {member.online ? `RTT ~${latestRtt}ms` : '无实时流'}
                            </span>
                          </div>
                        </div>
                      </div>

                      {/* 4. cmesh Networking Footer: Endpoint + Quick Probe Actions */}
                      <div
                        className={`mt-3 pt-2.5 border-t flex items-center justify-between gap-2 text-[11px] font-mono ${dividerClass}`}
                        onClick={(e) => e.stopPropagation()}
                      >
                        <span className={`truncate tabular-nums ${textMutedClass}`}>
                          {member.endpoints.length > 0
                            ? member.endpoints[0]
                            : '◆ 未公告入站端点 (仅出站)'}
                        </span>
                        <div className="flex items-center gap-1.5 shrink-0">
                          <button
                            disabled={!member.online || isPinging}
                            onClick={() => {
                              onSelectZone(member.zone_id);
                              onSelectDevice(member.device_id);
                              runProbeAction(member, 'ping');
                            }}
                            className={`px-2 py-0.5 rounded-md border transition-colors ${
                              !member.online
                                ? 'opacity-40 cursor-not-allowed border-slate-400/40'
                                : isDark
                                ? 'border-white/15 hover:border-sky-400 text-slate-200 hover:text-sky-300 cursor-pointer'
                                : 'border-slate-300/80 bg-white/60 hover:border-sky-600 text-slate-700 hover:text-sky-700 cursor-pointer'
                            }`}
                          >
                            {isPinging ? '测速中' : 'Ping'}
                          </button>
                          <button
                            onClick={() => onSwitchTo3DCockpit(member.zone_id, member.device_id)}
                            className={`px-2 py-0.5 rounded-md border transition-colors cursor-pointer ${
                              isDark
                                ? 'border-white/15 hover:border-sky-400 text-slate-200 hover:text-sky-300'
                                : 'border-slate-300/80 bg-white/60 hover:border-sky-600 text-slate-700 hover:text-sky-700'
                            }`}
                          >
                            3D定位
                          </button>
                        </div>
                      </div>
                    </div>
                  );
                })}
              </div>
            ) : (
              /* ---------- COMPACT STREAM LIST VIEW (LIST VIEW) ---------- */
              <div className={`rounded-xl overflow-hidden ${cardClass}`}>
                <div className="overflow-x-auto">
                  <table className="w-full text-left border-collapse">
                    <thead>
                      <tr
                        className={`border-b text-[11px] font-mono font-semibold ${dividerClass} ${textMutedClass}`}
                      >
                        <th className="py-2.5 px-3.5">状态</th>
                        <th className="py-2.5 px-3.5">节点名称 / 地区</th>
                        <th className="py-2.5 px-3.5">运行时 / 系统</th>
                        <th className="py-2.5 px-3.5">在线时长</th>
                        <th className="py-2.5 px-3.5">CPU</th>
                        <th className="py-2.5 px-3.5">内存 (RAM)</th>
                        <th className="py-2.5 px-3.5">实时网络 (↑/↓)</th>
                        <th className="py-2.5 px-3.5">组网端点</th>
                        <th className="py-2.5 px-3.5 text-right">操作</th>
                      </tr>
                    </thead>
                    <tbody className={`divide-y text-xs font-mono ${dividerClass}`}>
                      {filteredMembers.map((member) => {
                        const st = getDeviceStatusPresentation(member);
                        const t =
                          telemetryMap[member.device_id] || INITIAL_TELEMETRY.dev_01_anchor_hk;
                        const isSelected = selectedDeviceId === member.device_id;
                        const cpuPct = member.online ? t.cpuPercent : 0;
                        const memPct = member.online
                          ? Math.round((t.memUsedGb / t.memTotalGb) * 100)
                          : 0;

                        return (
                          <tr
                            key={member.device_id}
                            onClick={() => {
                              onSelectZone(member.zone_id);
                              onSelectDevice(isSelected ? null : member.device_id);
                            }}
                            className={`transition-colors cursor-pointer ${
                              isSelected
                                ? 'bg-sky-500/15'
                                : isDark
                                ? 'hover:bg-white/[0.04]'
                                : 'hover:bg-white/60'
                            }`}
                          >
                            <td className="py-2.5 px-3.5 whitespace-nowrap">
                              <span
                                className={`inline-flex items-center gap-1.5 font-semibold ${st.tone}`}
                              >
                                <span className={`w-2 h-2 rounded-full ${st.dotClass}`} />
                                <span>
                                  {st.glyph} {st.label}
                                </span>
                              </span>
                            </td>
                            <td className="py-2.5 px-3.5 whitespace-nowrap">
                              <div className={`font-bold ${textPrimaryClass}`}>
                                {t.flag} {member.display_name}
                              </div>
                              <div className={`text-[10px] ${textMutedClass}`}>
                                {member.geo_city}
                              </div>
                            </td>
                            <td className={`py-2.5 px-3.5 whitespace-nowrap ${textMutedClass}`}>
                              {t.osDistro}
                            </td>
                            <td
                              className={`py-2.5 px-3.5 whitespace-nowrap tabular-nums ${textMutedClass}`}
                            >
                              {t.uptimeStr}
                            </td>
                            <td className="py-2.5 px-3.5 whitespace-nowrap w-28">
                              <div className="flex items-center justify-between text-[11px] tabular-nums mb-1">
                                <span>{member.online ? `${cpuPct}%` : '—'}</span>
                              </div>
                              <div className="h-1.5 w-20 rounded-full bg-slate-500/20 overflow-hidden">
                                <div
                                  className={`h-full rounded-full transition-all duration-500 ${getProgressBarColor(
                                    cpuPct,
                                    member.online
                                  )}`}
                                  style={{ width: `${cpuPct}%` }}
                                />
                              </div>
                            </td>
                            <td className="py-2.5 px-3.5 whitespace-nowrap w-28">
                              <div className="flex items-center justify-between text-[11px] tabular-nums mb-1">
                                <span>{member.online ? `${memPct}%` : '—'}</span>
                              </div>
                              <div className="h-1.5 w-20 rounded-full bg-slate-500/20 overflow-hidden">
                                <div
                                  className={`h-full rounded-full transition-all duration-500 ${getProgressBarColor(
                                    memPct,
                                    member.online
                                  )}`}
                                  style={{ width: `${memPct}%` }}
                                />
                              </div>
                            </td>
                            <td className="py-2.5 px-3.5 whitespace-nowrap tabular-nums">
                              <span className={isDark ? 'text-sky-400' : 'text-sky-700 font-semibold'}>
                                ↑{member.online ? t.netUpMbps.toFixed(1) : '0.0'}M
                              </span>{' '}
                              <span
                                className={
                                  isDark ? 'text-emerald-400' : 'text-emerald-700 font-semibold'
                                }
                              >
                                ↓{member.online ? t.netDownMbps.toFixed(1) : '0.0'}M
                              </span>
                            </td>
                            <td
                              className={`py-2.5 px-3.5 whitespace-nowrap tabular-nums ${textMutedClass}`}
                            >
                              {member.endpoints[0] || '无入站端点'}
                            </td>
                            <td
                              className="py-2.5 px-3.5 whitespace-nowrap text-right"
                              onClick={(e) => e.stopPropagation()}
                            >
                              <button
                                onClick={() =>
                                  onSwitchTo3DCockpit(member.zone_id, member.device_id)
                                }
                                className={`px-2 py-0.5 rounded-md border text-[11px] cursor-pointer ${
                                  isDark
                                    ? 'border-sky-400/40 text-sky-300 hover:bg-sky-500/15'
                                    : 'border-sky-600/40 text-sky-700 hover:bg-sky-50'
                                }`}
                              >
                                3D地球
                              </button>
                            </td>
                          </tr>
                        );
                      })}
                    </tbody>
                  </table>
                </div>
              </div>
            )}

            {/* Inline Action Output Toast */}
            {actionOutput && (
              <div className={`p-3.5 rounded-xl flex items-center justify-between gap-4 text-xs font-mono ${cardClass}`}>
                <div className="min-w-0">
                  <div className={`font-semibold ${isDark ? 'text-sky-400' : 'text-sky-700'}`}>
                    {actionOutput.title}
                  </div>
                  <div className={`mt-0.5 truncate ${textPrimaryClass}`}>
                    {actionOutput.detail}
                  </div>
                </div>
                <button
                  onClick={() => setActionOutput(null)}
                  className={`text-[11px] shrink-0 cursor-pointer ${textMutedClass} hover:${textPrimaryClass}`}
                >
                  关闭回显
                </button>
              </div>
            )}

            {/* ================= 3. BOTTOM SERVICE AVAILABILITY & MULTI-PATH PING MONITOR (底部服务拨测区) ================= */}
            <div className={`rounded-xl overflow-hidden ${cardClass}`}>
              <button
                onClick={() => setShowServiceMonitor(!showServiceMonitor)}
                className={`w-full px-4 py-3 flex items-center justify-between text-xs font-semibold cursor-pointer ${
                  isDark ? 'hover:bg-white/[0.02]' : 'hover:bg-white/40'
                }`}
              >
                <div className="flex items-center gap-2">
                  <Gauge className={`w-4 h-4 ${isDark ? 'text-emerald-400' : 'text-emerald-600'}`} />
                  <span className={textPrimaryClass}>
                    服务可用性与跨洲组网拨测监控（UDP 锚点心跳 / P2P 延迟抖动序列 / 丢包率）
                  </span>
                </div>
                <div className={`flex items-center gap-2 font-mono text-[11px] ${textMutedClass}`}>
                  <span>独立拨测视图</span>
                  {showServiceMonitor ? (
                    <ChevronUp className="w-4 h-4" />
                  ) : (
                    <ChevronDown className="w-4 h-4" />
                  )}
                </div>
              </button>

              {showServiceMonitor && (
                <div className={`p-4 border-t space-y-2.5 ${dividerClass}`}>
                  {meshData.members.map((m) => {
                    const t =
                      telemetryMap[m.device_id] || INITIAL_TELEMETRY.dev_01_anchor_hk;
                    const avgRtt = Math.round(
                      t.pingSeriesDirect.reduce((a, b) => a + b, 0) /
                        t.pingSeriesDirect.length
                    );
                    return (
                      <div
                        key={m.device_id}
                        className={`p-3 rounded-xl flex flex-col md:flex-row md:items-center justify-between gap-3 ${subtleBgClass}`}
                      >
                        <div className="flex items-center gap-2.5 min-w-[210px]">
                          <span>{t.flag}</span>
                          <div>
                            <div className={`text-xs font-mono font-bold ${textPrimaryClass}`}>
                              {m.display_name}
                            </div>
                            <div className={`text-[11px] font-mono ${textMutedClass}`}>
                              {m.is_anchor
                                ? '香港锚点信标 (UDP 41820)'
                                : m.endpoints.length > 0
                                ? 'Direct P2P 拨测探针'
                                : 'Outbound 协调通道拨测'}
                            </div>
                          </div>
                        </div>

                        {/* 12-Slot Availability & Latency Micro-Bars */}
                        <div className="flex items-center gap-1 flex-1 max-w-md">
                          {t.pingSeriesDirect.map((rtt, idx) => {
                            const barColor = !m.online
                              ? 'bg-slate-500/35'
                              : rtt > 65
                              ? 'bg-amber-400'
                              : 'bg-emerald-500';
                            return (
                              <div
                                key={idx}
                                title={`${m.display_name}: ${m.online ? `${rtt} ms` : '离线'}`}
                                className={`h-4 flex-1 rounded-xs transition-all duration-300 ${barColor}`}
                              />
                            );
                          })}
                        </div>

                        <div className="flex items-center gap-4 font-mono text-xs tabular-nums shrink-0">
                          <div>
                            <span className={textMutedClass}>SLA: </span>
                            <span
                              className={`font-semibold ${
                                isDark ? 'text-emerald-400' : 'text-emerald-700'
                              }`}
                            >
                              {t.slaPercent}%
                            </span>
                          </div>
                          <div>
                            <span className={textMutedClass}>丢包: </span>
                            <span
                              className={
                                t.packetLossPercent > 0.5 ? 'text-amber-500' : textPrimaryClass
                              }
                            >
                              {m.online ? `${t.packetLossPercent}%` : '—'}
                            </span>
                          </div>
                          <div className="w-20 text-right">
                            <span className={textMutedClass}>RTT: </span>
                            <span className={`font-bold ${textPrimaryClass}`}>
                              {m.online ? `${avgRtt}ms` : '离线'}
                            </span>
                          </div>
                        </div>
                      </div>
                    );
                  })}
                </div>
              )}
            </div>
          </div>

          {/* ================= 4. DRILL-DOWN DRAWER (点击节点展开下钻详情与三路延迟对比曲线) ================= */}
          {selectedDevice &&
            (() => {
              const t =
                telemetryMap[selectedDevice.device_id] || INITIAL_TELEMETRY.dev_01_anchor_hk;
              return (
                <aside
                  className={`w-full xl:w-[440px] shrink-0 rounded-xl overflow-hidden ${cardClass}`}
                >
                  {/* Drawer Header */}
                  <div
                    className={`px-4 py-3 border-b flex items-center justify-between ${dividerClass}`}
                  >
                    <div className="flex items-center gap-2 text-xs font-mono font-bold">
                      <Box className="w-4 h-4 text-sky-500" />
                      <span className={textPrimaryClass}>
                        {t.flag} {selectedDevice.display_name} · 节点下钻详情
                      </span>
                    </div>
                    <div className="flex items-center gap-2">
                      <button
                        onClick={() =>
                          onSwitchTo3DCockpit(selectedDevice.zone_id, selectedDevice.device_id)
                        }
                        className={`text-xs hover:underline font-medium flex items-center gap-1 cursor-pointer ${
                          isDark ? 'text-sky-400' : 'text-sky-700'
                        }`}
                      >
                        <Globe className="w-3.5 h-3.5" />
                        <span>3D 地球定位</span>
                      </button>
                      <button
                        onClick={() => onSelectDevice(null)}
                        className={`text-xs pl-2 border-l cursor-pointer ${dividerClass} ${textMutedClass} hover:${textPrimaryClass}`}
                      >
                        关闭
                      </button>
                    </div>
                  </div>

                  <div className="p-4 space-y-4">
                    {/* Identity & Online Toggle */}
                    <div
                      className={`flex items-start justify-between gap-2 pb-3 border-b ${dividerClass}`}
                    >
                      <div>
                        <div className={`text-[11px] font-mono ${textMutedClass}`}>
                          {selectedDevice.device_id} · {selectedDevice.geo_city}
                        </div>
                        <div className={`text-base font-bold font-mono mt-0.5 ${textPrimaryClass}`}>
                          {selectedDevice.display_name}
                        </div>
                        <div className={`text-xs mt-0.5 ${textMutedClass}`}>
                          {t.osDistro} ({t.kernelArch})
                        </div>
                      </div>
                      <button
                        onClick={() => onToggleDeviceOnline(selectedDevice.device_id)}
                        className={`px-2.5 py-1.5 rounded-lg text-xs font-medium border flex items-center gap-1 transition-colors cursor-pointer shrink-0 ${
                          selectedDevice.online
                            ? isDark
                              ? 'bg-slate-800/80 border-white/15 text-slate-200 hover:bg-slate-700'
                              : 'bg-white border-slate-300 text-slate-700 hover:bg-slate-100'
                            : 'bg-emerald-600 border-emerald-500 text-white hover:bg-emerald-500'
                        }`}
                      >
                        <Power className="w-3.5 h-3.5" />
                        <span>{selectedDevice.online ? '模拟离线' : '恢复上线'}</span>
                      </button>
                    </div>

                    {/* Drill-down Multi-Line Chart (3-Route Ping Comparison vs Resource Load) */}
                    <div className={`p-3 rounded-xl space-y-2.5 ${subtleBgClass}`}>
                      <div className="flex items-center justify-between text-xs">
                        <span className={`font-semibold ${textPrimaryClass}`}>
                          {drilldownTab === 'ping_chart'
                            ? '三路组网通道 RTT 延迟对比曲线 (ms)'
                            : '容器 CPU 实时负载波动曲线 (%)'}
                        </span>
                        <div className="flex items-center gap-1 text-[11px] font-mono">
                          <button
                            onClick={() => setDrilldownTab('ping_chart')}
                            className={`px-2 py-0.5 rounded-md cursor-pointer ${
                              drilldownTab === 'ping_chart'
                                ? 'bg-sky-500/20 text-sky-500 font-semibold'
                                : textMutedClass
                            }`}
                          >
                            三路延迟
                          </button>
                          <button
                            onClick={() => setDrilldownTab('load_chart')}
                            className={`px-2 py-0.5 rounded-md cursor-pointer ${
                              drilldownTab === 'load_chart'
                                ? 'bg-sky-500/20 text-sky-500 font-semibold'
                                : textMutedClass
                            }`}
                          >
                            CPU 负载
                          </button>
                        </div>
                      </div>

                      {/* SVG Multi-Line Chart */}
                      <div className="pt-1">
                        {drilldownTab === 'ping_chart' ? (
                          <div>
                            <svg viewBox="0 0 320 96" className="w-full h-24 overflow-visible">
                              <line
                                x1="0"
                                y1="20"
                                x2="320"
                                y2="20"
                                stroke="currentColor"
                                strokeOpacity="0.1"
                              />
                              <line
                                x1="0"
                                y1="50"
                                x2="320"
                                y2="50"
                                stroke="currentColor"
                                strokeOpacity="0.1"
                              />
                              <line
                                x1="0"
                                y1="80"
                                x2="320"
                                y2="80"
                                stroke="currentColor"
                                strokeOpacity="0.1"
                              />
                              {selectedDevice.online &&
                                (() => {
                                  const maxVal = Math.max(
                                    ...t.pingSeriesDirect,
                                    ...t.pingSeriesAnchor,
                                    ...t.pingSeriesRelay,
                                    80
                                  );
                                  const toPts = (arr: number[]) =>
                                    arr
                                      .map((v, i) => {
                                        const x = (i / (arr.length - 1)) * 320;
                                        const y = 86 - (v / maxVal) * 72;
                                        return `${x.toFixed(1)},${y.toFixed(1)}`;
                                      })
                                      .join(' ');
                                  return (
                                    <>
                                      <polyline
                                        fill="none"
                                        stroke="#10b981"
                                        strokeWidth="2"
                                        points={toPts(t.pingSeriesDirect)}
                                      />
                                      <polyline
                                        fill="none"
                                        stroke="#0284c7"
                                        strokeWidth="2"
                                        points={toPts(t.pingSeriesAnchor)}
                                      />
                                      <polyline
                                        fill="none"
                                        stroke="#d97706"
                                        strokeWidth="1.75"
                                        strokeDasharray="4 3"
                                        points={toPts(t.pingSeriesRelay)}
                                      />
                                    </>
                                  );
                                })()}
                            </svg>
                            <div className="flex items-center justify-between text-[11px] font-mono tabular-nums pt-1">
                              <span className={isDark ? 'text-emerald-400' : 'text-emerald-700'}>
                                ● 欧洲直连: {t.pingSeriesDirect[t.pingSeriesDirect.length - 1]}ms
                              </span>
                              <span className={isDark ? 'text-sky-400' : 'text-sky-700'}>
                                ● 香港锚点: {t.pingSeriesAnchor[t.pingSeriesAnchor.length - 1]}ms
                              </span>
                              <span className={isDark ? 'text-amber-400' : 'text-amber-700'}>
                                ● 中继通道: {t.pingSeriesRelay[t.pingSeriesRelay.length - 1]}ms
                              </span>
                            </div>
                          </div>
                        ) : (
                          <div>
                            <svg viewBox="0 0 320 96" className="w-full h-24 overflow-visible">
                              <line
                                x1="0"
                                y1="20"
                                x2="320"
                                y2="20"
                                stroke="currentColor"
                                strokeOpacity="0.1"
                              />
                              <line
                                x1="0"
                                y1="50"
                                x2="320"
                                y2="50"
                                stroke="currentColor"
                                strokeOpacity="0.1"
                              />
                              <line
                                x1="0"
                                y1="80"
                                x2="320"
                                y2="80"
                                stroke="currentColor"
                                strokeOpacity="0.1"
                              />
                              {selectedDevice.online && (
                                <polyline
                                  fill="none"
                                  stroke="#0284c7"
                                  strokeWidth="2"
                                  points={t.sparkline
                                    .map((v, i) => {
                                      const x = (i / (t.sparkline.length - 1)) * 320;
                                      const y = 86 - (v / 100) * 72;
                                      return `${x.toFixed(1)},${y.toFixed(1)}`;
                                    })
                                    .join(' ')}
                                />
                              )}
                            </svg>
                            <div
                              className={`flex items-center justify-between text-[11px] font-mono tabular-nums pt-1 ${textMutedClass}`}
                            >
                              <span>Load 1m: {t.load1}</span>
                              <span>CPU: {t.cpuPercent}%</span>
                              <span>
                                RAM: {t.memUsedGb.toFixed(1)} / {t.memTotalGb} GB
                              </span>
                            </div>
                          </div>
                        )}
                      </div>
                    </div>

                    {/* 4D Orthogonal State Grid */}
                    <div className="space-y-1.5">
                      <div className={`text-xs font-semibold ${textPrimaryClass}`}>
                        四维正交组网状态（Membership / Liveness / Endpoints / Transport）
                      </div>
                      <div className="grid grid-cols-2 gap-2 text-xs">
                        <div className={`p-2.5 rounded-xl ${subtleBgClass}`}>
                          <div className={`text-[11px] ${textMutedClass}`}>1. 成员关系</div>
                          <div
                            className={`font-mono font-semibold mt-0.5 flex items-center gap-1 ${textPrimaryClass}`}
                          >
                            <CheckCircle2 className="w-3.5 h-3.5 text-sky-500" />
                            <span>在册 (v{meshData.roster_version})</span>
                          </div>
                        </div>
                        <div className={`p-2.5 rounded-xl ${subtleBgClass}`}>
                          <div className={`text-[11px] ${textMutedClass}`}>2. 当前可达性</div>
                          <div
                            className={`font-mono font-semibold mt-0.5 ${
                              selectedDevice.online
                                ? isDark
                                  ? 'text-emerald-400'
                                  : 'text-emerald-700'
                                : textMutedClass
                            }`}
                          >
                            {selectedDevice.online ? '● 在线可达' : '○ 离线 (未移出)'}
                          </div>
                        </div>
                        <div className={`p-2.5 rounded-xl ${subtleBgClass}`}>
                          <div className={`text-[11px] ${textMutedClass}`}>3. 公告入站端点</div>
                          <div
                            className={`font-mono font-semibold mt-0.5 ${
                              selectedDevice.endpoints.length > 0
                                ? textPrimaryClass
                                : isDark
                                ? 'text-amber-400'
                                : 'text-amber-700'
                            }`}
                          >
                            {selectedDevice.endpoints.length > 0
                              ? `${selectedDevice.endpoints.length} 个可用端点`
                              : '◆ 未公告端点'}
                          </div>
                        </div>
                        <div className={`p-2.5 rounded-xl ${subtleBgClass}`}>
                          <div className={`text-[11px] ${textMutedClass}`}>4. 传输通道</div>
                          <div className={`font-mono font-semibold mt-0.5 ${textPrimaryClass}`}>
                            {!selectedDevice.online
                              ? '不可用'
                              : selectedDevice.transport === 'direct'
                              ? 'Direct P2P 直连'
                              : selectedDevice.transport === 'relay'
                              ? 'Anchor Relay 中继'
                              : 'Pending 仅出站'}
                          </div>
                        </div>
                      </div>
                    </div>

                    {/* Userspace Operations Workbench */}
                    <div className={`p-3 rounded-xl space-y-2.5 ${subtleBgClass}`}>
                      <div className="flex items-center justify-between text-xs">
                        <span
                          className={`font-semibold flex items-center gap-1.5 ${textPrimaryClass}`}
                        >
                          <ShieldCheck className="w-3.5 h-3.5 text-sky-500" />
                          <span>受控用户态探测与操作台</span>
                        </span>
                        <span className={`font-mono text-[10px] ${textMutedClass}`}>
                          实测按需触发
                        </span>
                      </div>

                      <div className="grid grid-cols-2 gap-2">
                        <button
                          disabled={!selectedDevice.online || actionLoading !== null}
                          onClick={() => runProbeAction(selectedDevice, 'ping')}
                          className={`py-1.5 px-2.5 rounded-lg text-xs font-medium flex items-center justify-center gap-1.5 border transition-colors ${
                            !selectedDevice.online
                              ? 'opacity-40 cursor-not-allowed border-slate-500/30'
                              : isDark
                              ? 'bg-slate-900/70 hover:bg-sky-500/15 text-slate-100 border-white/15 hover:border-sky-400 cursor-pointer'
                              : 'bg-white/80 hover:bg-sky-50 text-slate-800 border-slate-300 hover:border-sky-500 cursor-pointer'
                          }`}
                        >
                          <Activity className="w-3.5 h-3.5 text-sky-500" />
                          <span>测量延迟 (RTT)</span>
                        </button>

                        <button
                          disabled={!selectedDevice.online || actionLoading !== null}
                          onClick={() => runProbeAction(selectedDevice, 'file_transfer')}
                          className={`py-1.5 px-2.5 rounded-lg text-xs font-medium flex items-center justify-center gap-1.5 border transition-colors ${
                            !selectedDevice.online
                              ? 'opacity-40 cursor-not-allowed border-slate-500/30'
                              : isDark
                              ? 'bg-slate-900/70 hover:bg-sky-500/15 text-slate-100 border-white/15 hover:border-sky-400 cursor-pointer'
                              : 'bg-white/80 hover:bg-sky-50 text-slate-800 border-slate-300 hover:border-sky-500 cursor-pointer'
                          }`}
                        >
                          <FileUp className="w-3.5 h-3.5 text-sky-500" />
                          <span>传大文件分片</span>
                        </button>
                      </div>

                      {/* Send Encrypted Message */}
                      <div className="flex items-center gap-1.5">
                        <input
                          type="text"
                          disabled={!selectedDevice.online}
                          value={msgInput}
                          onChange={(e) => setMsgInput(e.target.value)}
                          className={`flex-1 px-2.5 py-1 text-xs border rounded-lg font-mono ${
                            isDark
                              ? 'bg-slate-900/90 border-white/15 text-slate-100'
                              : 'bg-white/90 border-slate-300 text-slate-800'
                          }`}
                        />
                        <button
                          disabled={!selectedDevice.online || actionLoading !== null}
                          onClick={() => runProbeAction(selectedDevice, 'message')}
                          className={`px-2.5 py-1 rounded-lg text-xs font-medium flex items-center gap-1 shrink-0 ${
                            !selectedDevice.online
                              ? 'opacity-40 cursor-not-allowed bg-slate-700 text-slate-400'
                              : 'bg-sky-600 hover:bg-sky-500 text-white cursor-pointer'
                          }`}
                        >
                          <Send className="w-3 h-3" />
                          <span>发送</span>
                        </button>
                      </div>

                      {/* Whitelist Remote Command */}
                      <div className="flex items-center gap-1.5">
                        <select
                          disabled={!selectedDevice.online}
                          value={selectedCmd}
                          onChange={(e) => setSelectedCmd(e.target.value)}
                          className={`flex-1 px-2 py-1 text-xs border rounded-lg font-mono ${
                            isDark
                              ? 'bg-slate-900/90 border-white/15 text-slate-100'
                              : 'bg-white/90 border-slate-300 text-slate-800'
                          }`}
                        >
                          {selectedDevice.container_config.whitelist_cmds.map((cmd) => (
                            <option key={cmd} value={cmd}>
                              {cmd}
                            </option>
                          ))}
                        </select>
                        <button
                          disabled={!selectedDevice.online || actionLoading !== null}
                          onClick={() => runProbeAction(selectedDevice, 'exec_whitelist')}
                          className={`px-2.5 py-1 rounded-lg text-xs font-medium flex items-center gap-1 shrink-0 ${
                            !selectedDevice.online
                              ? 'opacity-40 cursor-not-allowed bg-slate-700 text-slate-400'
                              : 'bg-emerald-600 hover:bg-emerald-500 text-white cursor-pointer'
                          }`}
                        >
                          <Terminal className="w-3 h-3" />
                          <span>执行</span>
                        </button>
                      </div>
                    </div>

                    {/* Collapsible Container Internal Configuration */}
                    <div className={`border rounded-xl overflow-hidden ${dividerClass}`}>
                      <button
                        onClick={() => setShowConfigAccordion(!showConfigAccordion)}
                        className={`w-full px-3 py-2 flex items-center justify-between text-xs font-semibold cursor-pointer ${subtleBgClass} ${textPrimaryClass}`}
                      >
                        <span className="flex items-center gap-1.5">
                          <Sliders className="w-3.5 h-3.5 text-sky-500" />
                          <span>容器内部零特权运行配置</span>
                        </span>
                        {showConfigAccordion ? (
                          <ChevronUp className="w-4 h-4" />
                        ) : (
                          <ChevronDown className="w-4 h-4" />
                        )}
                      </button>
                      {showConfigAccordion && (
                        <div
                          className={`p-3 text-[11px] font-mono space-y-1.5 border-t ${dividerClass}`}
                        >
                          <div className="flex justify-between">
                            <span className={textMutedClass}>容器运行时:</span>
                            <span className={textPrimaryClass}>
                              {selectedDevice.container_config.runtime}
                            </span>
                          </div>
                          <div className="flex justify-between">
                            <span className={textMutedClass}>进程权限:</span>
                            <span
                              className={`font-semibold ${
                                isDark ? 'text-emerald-400' : 'text-emerald-700'
                              }`}
                            >
                              {selectedDevice.container_config.privileges}
                            </span>
                          </div>
                          <div className="flex justify-between">
                            <span className={textMutedClass}>CAP_NET_ADMIN / TUN:</span>
                            <span
                              className={`font-semibold ${
                                isDark ? 'text-emerald-400' : 'text-emerald-700'
                              }`}
                            >
                              false / false (纯用户态)
                            </span>
                          </div>
                          <div className="flex justify-between">
                            <span className={textMutedClass}>入站端口要求:</span>
                            <span className={textPrimaryClass}>
                              {selectedDevice.container_config.inbound_ports}
                            </span>
                          </div>
                        </div>
                      )}
                    </div>

                    {/* Collapsible Diagnostic Reason & Actionable Suggestion */}
                    <div className={`border rounded-xl overflow-hidden ${dividerClass}`}>
                      <button
                        onClick={() => setShowDiagnosticsAccordion(!showDiagnosticsAccordion)}
                        className={`w-full px-3 py-2 flex items-center justify-between text-xs font-semibold cursor-pointer ${subtleBgClass} ${textPrimaryClass}`}
                      >
                        <span className="flex items-center gap-1.5">
                          <AlertTriangle className="w-3.5 h-3.5 text-amber-500" />
                          <span>状态原因解释与排查建议</span>
                        </span>
                        {showDiagnosticsAccordion ? (
                          <ChevronUp className="w-4 h-4" />
                        ) : (
                          <ChevronDown className="w-4 h-4" />
                        )}
                      </button>
                      {showDiagnosticsAccordion && (
                        <div
                          className={`p-3 text-xs space-y-1.5 border-t leading-relaxed ${dividerClass}`}
                        >
                          <div className={textPrimaryClass}>
                            <strong>状态原因：</strong>
                            {selectedDevice.diagnostic_reason}
                          </div>
                          <div className={textMutedClass}>
                            <strong>建议动作：</strong>
                            {selectedDevice.diagnostic_suggestion}
                          </div>
                        </div>
                      )}
                    </div>
                  </div>
                </aside>
              );
            })()}
        </div>
      </div>
    </div>
  );
};
