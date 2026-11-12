import React, { useState } from 'react';
import { MeshApiResponse, MeshMember } from '../data/meshSnapshots';
import {
  Layers,
  Server,
  Box,
  ChevronRight,
  ArrowLeft,
  Send,
  FileUp,
  Activity,
  Terminal,
  Power,
  AlertTriangle,
  CheckCircle2,
  Sliders,
  ShieldCheck,
  ChevronDown,
  ChevronUp,
} from 'lucide-react';

interface NestedHierarchyInspectorProps {
  meshData: MeshApiResponse;
  selectedZoneId: string | null;
  selectedDeviceId: string | null;
  onSelectZone: (zoneId: string | null) => void;
  onSelectDevice: (deviceId: string | null) => void;
  onToggleDeviceOnline: (deviceId: string) => void;
  onTriggerCommEvent: (fromId: string, toId: string, label: string) => void;
}

export const NestedHierarchyInspector: React.FC<NestedHierarchyInspectorProps> = ({
  meshData,
  selectedZoneId,
  selectedDeviceId,
  onSelectZone,
  onSelectDevice,
  onToggleDeviceOnline,
  onTriggerCommEvent,
}) => {
  const [rosterFilter, setRosterFilter] = useState<'all' | 'online' | 'offline' | 'no_endpoint'>(
    'all'
  );
  const [showConfigAccordion, setShowConfigAccordion] = useState<boolean>(false);
  const [showDiagnosticsDrawer, setShowDiagnosticsDrawer] = useState<boolean>(false);

  // Action feedback state for verified capabilities
  const [actionLoading, setActionLoading] = useState<string | null>(null);
  const [actionOutput, setActionOutput] = useState<{
    title: string;
    timestamp: string;
    detail: string;
  } | null>(null);
  const [msgInput, setMsgInput] = useState<string>('PING_CHECK: 请求同步容器构建状态');
  const [selectedCmd, setSelectedCmd] = useState<string>('cmesh status --json');

  const selectedZone = meshData.zones.find((z) => z.zone_id === selectedZoneId) || null;
  const selectedDevice = meshData.members.find((m) => m.device_id === selectedDeviceId) || null;

  const filteredMembers = meshData.members.filter((m) => {
    if (selectedZoneId && m.zone_id !== selectedZoneId) return false;
    if (rosterFilter === 'online') return m.online;
    if (rosterFilter === 'offline') return !m.online;
    if (rosterFilter === 'no_endpoint') return m.endpoints.length === 0;
    return true;
  });

  const executeDeviceAction = async (
    member: MeshMember,
    action: 'ping' | 'message' | 'file_transfer' | 'exec_whitelist'
  ) => {
    setActionLoading(action);
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
          `RTT 测量完成: macbook-studio-local ⇄ ${member.display_name} (${json.result.avg_rtt_ms} ms)`
        );
        setActionOutput({
          title: `用户态 RTT 延迟探测 — ${member.display_name}`,
          timestamp: json.timestamp,
          detail: `模式: ${json.result.mode}\n样本: ${json.result.samples_ms.join(' ms, ')} ms\n平均往返延迟: ${json.result.avg_rtt_ms} ms (丢包率 ${json.result.packet_loss})`,
        });
      } else if (action === 'message') {
        onTriggerCommEvent(
          'dev_02_local_ws',
          member.device_id,
          `加密消息已送达: -> ${member.display_name} [${json.result.msg_id}]`
        );
        setActionOutput({
          title: `点对点消息发送成功 (${json.result.encrypted})`,
          timestamp: json.timestamp,
          detail: `${json.result.echo}\n消息回执 ID: ${json.result.msg_id}`,
        });
      } else if (action === 'file_transfer') {
        onTriggerCommEvent(
          'dev_02_local_ws',
          member.device_id,
          `大文件分片校验完成: -> ${member.display_name} (256.0 MB)`
        );
        setActionOutput({
          title: `用户态大文件流式传输 — ${json.result.file_name}`,
          timestamp: json.timestamp,
          detail: `文件大小: ${json.result.file_size} · 分片校验: ${json.result.chunks_verified}\nSHA-256: ${json.result.sha256.slice(0, 32)}...`,
        });
      } else if (action === 'exec_whitelist') {
        onTriggerCommEvent(
          'dev_02_local_ws',
          member.device_id,
          `受控白名单命令执行: ${selectedCmd} @ ${member.display_name}`
        );
        setActionOutput({
          title: `受控远程白名单操作 (${json.result.command})`,
          timestamp: json.timestamp,
          detail: json.result.stdout,
        });
      }
    } catch {
      setActionOutput({
        title: '本地回显 (离线或接口重试中)',
        timestamp: new Date().toISOString(),
        detail: `已向容器 [${member.display_name}] 发送用户态请求。`,
      });
    } finally {
      setActionLoading(null);
    }
  };

  const renderTransportLabel = (member: MeshMember) => {
    if (!member.online) return '不可用 (Unavailable · 离线在册)';
    if (member.transport === 'direct') return '点对点直连 (Direct P2P)';
    if (member.transport === 'relay') return '锚点受控中继 (Anchor Relay)';
    if (member.transport === 'pending') return '仅出站连接 / 待公告 (Pending)';
    return '不可用 (Unavailable)';
  };

  return (
    <aside className="w-full lg:w-[430px] shrink-0 bg-white border border-slate-200 rounded-lg flex flex-col justify-between overflow-hidden">
      {/* Top Panel Header: Nested Depth Breadcrumb */}
      <div className="px-4 py-3 border-b border-slate-200 bg-slate-50/70 flex items-center justify-between gap-2">
        <div className="flex items-center gap-1.5 text-xs font-medium text-slate-700 truncate">
          {!selectedZone && !selectedDevice && (
            <>
              <Layers className="w-4 h-4 text-sky-600 shrink-0" />
              <span className="font-semibold text-slate-900">
                第 1 层：全网机房与容器整体状况
              </span>
            </>
          )}
          {selectedZone && !selectedDevice && (
            <>
              <Server className="w-4 h-4 text-sky-600 shrink-0" />
              <span className="font-semibold text-slate-900 truncate">
                第 2 层：{selectedZone.zone_name}
              </span>
            </>
          )}
          {selectedDevice && (
            <>
              <Box className="w-4 h-4 text-sky-600 shrink-0" />
              <span className="font-semibold text-slate-900 font-mono truncate">
                第 3 层：{selectedDevice.display_name}
              </span>
            </>
          )}
        </div>

        {(selectedZone || selectedDevice) && (
          <button
            onClick={() => {
              setActionOutput(null);
              if (selectedDevice) {
                onSelectDevice(null);
              } else {
                onSelectZone(null);
              }
            }}
            className="px-2 py-1 text-xs font-medium text-sky-700 hover:bg-sky-50 rounded flex items-center gap-1 shrink-0 cursor-pointer"
          >
            <ArrowLeft className="w-3.5 h-3.5" />
            <span>返回上级</span>
          </button>
        )}
      </div>

      {/* Main Scrollable Content Area */}
      <div className="p-4 space-y-4 overflow-y-auto max-h-[620px]">
        {/* ================= LEVEL 1 & 2: COLLECTIVE OVERVIEW & ZONE DRILL-DOWN ================= */}
        {!selectedDevice && (
          <>
            {/* Selected Zone Header (only shown when a Zone is clicked) */}
            {selectedZone && (
              <div className="p-3 bg-slate-50 border border-slate-200 rounded-md text-xs text-slate-600">
                <div className="flex items-center justify-between">
                  <span className="font-semibold text-slate-900">{selectedZone.zone_name}</span>
                  <span className="font-mono text-[11px] text-sky-700">
                    {selectedZone.zone_code}
                  </span>
                </div>
                <div className="text-[11px] font-mono text-slate-500 mt-1">
                  {selectedZone.geo_coord_label} · {selectedZone.environment_type}
                </div>
              </div>
            )}

            {/* Zone Summary Cards (Only shown at Level 1 Root) */}
            {!selectedZone && (
              <div className="space-y-2">
                <div className="flex items-center justify-between text-xs text-slate-500">
                  <span className="font-medium text-slate-700">全球机房 / 地理区域</span>
                  <span className="font-mono">{meshData.zones.length} 个地理区</span>
                </div>
                <div className="grid grid-cols-1 gap-2">
                  {meshData.zones.map((zone) => {
                    const zMembers = meshData.members.filter((m) => m.zone_id === zone.zone_id);
                    const zOnline = zMembers.filter((m) => m.online).length;
                    return (
                      <button
                        key={zone.zone_id}
                        onClick={() => onSelectZone(zone.zone_id)}
                        title={zone.description}
                        className="w-full text-left p-2.5 rounded-md border border-slate-200 hover:border-sky-500 bg-white transition-all flex items-center justify-between group cursor-pointer"
                      >
                        <div className="min-w-0 pr-2">
                          <div className="text-xs font-semibold text-slate-900 truncate">
                            {zone.zone_name}
                          </div>
                          <div className="text-[11px] font-mono text-slate-500 mt-0.5">
                            {zone.geo_coord_label} · {zone.environment_type}
                          </div>
                        </div>
                        <div className="flex items-center gap-2 shrink-0">
                          <span className="text-xs font-mono font-bold text-slate-900 tabular-nums">
                            {zOnline}/{zMembers.length}
                          </span>
                          <ChevronRight className="w-4 h-4 text-slate-400 group-hover:text-sky-600 group-hover:translate-x-0.5 transition-transform" />
                        </div>
                      </button>
                    );
                  })}
                </div>
              </div>
            )}

            {/* Container Roster List with Interactive Filter Tabs */}
            <div className="space-y-2.5">
              <div className="flex items-center justify-between">
                <span className="text-xs font-medium text-slate-700">
                  {selectedZone ? '区域内嵌容器节点 (点击定位地球坐标)' : '全球在册节点列表 (点击旋转地球定位)'}
                </span>
                <span className="text-[11px] font-mono text-slate-500 tabular-nums">
                  {filteredMembers.length} / {meshData.members.length}
                </span>
              </div>

              {/* Interactive Segmented Filter Control */}
              <div className="flex items-center gap-1 p-1 bg-slate-100 rounded-md">
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
                    onClick={() => setRosterFilter(tab.key)}
                    className={`flex-1 py-1 px-2 text-xs font-medium rounded transition-colors cursor-pointer whitespace-nowrap ${
                      rosterFilter === tab.key
                        ? 'bg-white text-slate-900 shadow-2xs'
                        : 'text-slate-600 hover:text-slate-900'
                    }`}
                  >
                    {tab.label}
                  </button>
                ))}
              </div>

              {/* Container Node Cards (Clean & Minimal — Hover status symbol for meaning) */}
              <div className="space-y-1.5">
                {filteredMembers.map((member) => {
                  const hasEndpoints = member.endpoints.length > 0;
                  let glyph = '●';
                  let statusShort = '直连';
                  let statusColor = 'text-emerald-700';

                  if (!member.online) {
                    glyph = '○';
                    statusShort = '离线(在册)';
                    statusColor = 'text-slate-500';
                  } else if (!hasEndpoints) {
                    glyph = '◆';
                    statusShort = '无端点';
                    statusColor = 'text-amber-700';
                  } else if (member.transport === 'relay') {
                    glyph = '⇄';
                    statusShort = '中继';
                    statusColor = 'text-amber-700';
                  } else if (member.is_anchor) {
                    glyph = '◈';
                    statusShort = '公网锚点';
                    statusColor = 'text-sky-700';
                  }

                  return (
                    <div
                      key={member.device_id}
                      onClick={() => {
                        onSelectZone(member.zone_id);
                        onSelectDevice(member.device_id);
                        setActionOutput(null);
                      }}
                      className={`p-2.5 rounded-md border transition-all cursor-pointer group ${
                        !member.online
                          ? 'bg-slate-50/80 border-slate-200 hover:border-slate-400'
                          : 'bg-white border-slate-200 hover:border-sky-500'
                      }`}
                    >
                      <div className="flex items-center justify-between gap-2">
                        <div className="flex items-center gap-2 min-w-0">
                          {/* Hoverable Status Badge */}
                          <span
                            title={`${member.active_comm_summary} — ${member.diagnostic_reason}`}
                            className={`font-mono text-xs font-bold ${statusColor} cursor-help`}
                          >
                            {glyph}
                          </span>
                          <span className="font-mono text-xs font-semibold text-slate-900 truncate">
                            {member.display_name}
                          </span>
                          <span className="text-[11px] font-mono text-slate-500 truncate">
                            {member.geo_city}
                          </span>
                        </div>

                        <div className="flex items-center gap-2 shrink-0">
                          <span
                            title={`地球坐标: ${member.geo_coord_label} · 3D 建模: ${member.nat_type_label}`}
                            className={`text-[11px] font-mono ${statusColor}`}
                          >
                            {statusShort}
                          </span>
                          <ChevronRight className="w-3.5 h-3.5 text-slate-400 group-hover:text-sky-600" />
                        </div>
                      </div>
                    </div>
                  );
                })}
              </div>
            </div>
          </>
        )}

        {/* ================= LEVEL 3: DEEP CONTAINER / NODE INSPECTOR ================= */}
        {selectedDevice && (
          <div className="space-y-4">
            {/* Primary Device Identity & Liveness Toggle Bar */}
            <div className="p-3.5 bg-slate-50 border border-slate-200 rounded-md">
              <div className="flex items-start justify-between gap-2">
                <div>
                  <div className="text-xs font-mono text-slate-500">
                    ID: {selectedDevice.device_id} · {selectedDevice.geo_city} ({selectedDevice.geo_coord_label})
                  </div>
                  <h3 className="text-base font-bold text-slate-900 font-mono mt-0.5">
                    {selectedDevice.display_name}
                  </h3>
                  <div className="text-xs text-slate-600 mt-0.5">{selectedDevice.alias}</div>
                </div>

                {/* Simulate Online/Offline Transition Button */}
                <button
                  onClick={() => onToggleDeviceOnline(selectedDevice.device_id)}
                  className={`px-2.5 py-1.5 rounded text-xs font-medium border flex items-center gap-1.5 transition-colors cursor-pointer whitespace-nowrap ${
                    selectedDevice.online
                      ? 'bg-white border-slate-300 text-slate-700 hover:bg-slate-100'
                      : 'bg-emerald-600 border-emerald-600 text-white hover:bg-emerald-700'
                  }`}
                  title="模拟节点离线时，节点变为中性灰并保留在名册中，无红色灾难感"
                >
                  <Power className="w-3.5 h-3.5" />
                  <span>{selectedDevice.online ? '模拟离线' : '恢复上线'}</span>
                </button>
              </div>

              <div className="mt-2.5 pt-2.5 border-t border-slate-200/80 text-[11px] text-slate-600 space-y-1 font-mono">
                <div className="flex items-center justify-between">
                  <span>3D 建模形态:</span>
                  <span className="text-sky-800 font-semibold">
                    {selectedDevice.nat_type_label}
                  </span>
                </div>
                <div className="flex items-center justify-between text-slate-500">
                  <span>{selectedDevice.zone_name}</span>
                  <span>最后在线: {selectedDevice.last_seen.replace('T', ' ').slice(0, 19)}</span>
                </div>
              </div>
            </div>

            {/* Four-Dimensional Orthogonal State Matrix (Chapter 9 Core Requirement) */}
            <div className="space-y-2">
              <div className="flex items-center justify-between">
                <span className="text-xs font-semibold text-slate-800">
                  四维正交状态读取 (避免二元状态误导)
                </span>
                <span className="text-[11px] font-mono text-slate-500">
                  Roster v{meshData.roster_version}
                </span>
              </div>

              <div className="grid grid-cols-2 gap-2 text-xs">
                {/* 1. Membership */}
                <div className="p-2.5 border border-slate-200 rounded bg-white">
                  <div className="text-[11px] text-slate-500">1. Membership (成员关系)</div>
                  <div className="font-mono font-semibold text-slate-900 mt-1 flex items-center gap-1.5">
                    <CheckCircle2 className="w-3.5 h-3.5 text-sky-600 shrink-0" />
                    <span>已在网络名册中</span>
                  </div>
                  <div className="text-[11px] text-slate-500 mt-0.5">
                    即使离线或无端点仍属本网
                  </div>
                </div>

                {/* 2. Liveness */}
                <div className="p-2.5 border border-slate-200 rounded bg-white">
                  <div className="text-[11px] text-slate-500">2. Liveness (当前可达)</div>
                  <div
                    className={`font-mono font-semibold mt-1 ${
                      selectedDevice.online ? 'text-emerald-700' : 'text-slate-600'
                    }`}
                  >
                    {selectedDevice.online ? '● 当前在线可达' : '○ 当前离线 (未移除)'}
                  </div>
                  <div className="text-[11px] text-slate-500 mt-0.5">
                    {selectedDevice.online ? '心跳与控制信道正常' : '中性灰保留，非安全故障'}
                  </div>
                </div>

                {/* 3. Announcement */}
                <div className="p-2.5 border border-slate-200 rounded bg-white">
                  <div className="text-[11px] text-slate-500">3. Announcement (公告端点)</div>
                  <div
                    className={`font-mono font-semibold mt-1 ${
                      selectedDevice.endpoints.length > 0 ? 'text-slate-900' : 'text-amber-700'
                    }`}
                  >
                    {selectedDevice.endpoints.length > 0
                      ? `已公告 ${selectedDevice.endpoints.length} 个可拨地址`
                      : '◆ 无公告端点 (endpoints: [])'}
                  </div>
                  <div className="text-[11px] text-slate-500 mt-0.5">
                    {selectedDevice.endpoints.length > 0
                      ? '允许其他节点主动发起直连'
                      : '未开启 --listen 或待首次公告'}
                  </div>
                </div>

                {/* 4. Discovery & Transport */}
                <div className="p-2.5 border border-slate-200 rounded bg-white">
                  <div className="text-[11px] text-slate-500">4. 连接模式与发现同步</div>
                  <div
                    className={`font-mono font-semibold mt-1 ${
                      meshData.is_cached ? 'text-amber-700' : 'text-sky-700'
                    }`}
                  >
                    {meshData.is_cached
                      ? '▲ 缓存视图快照'
                      : renderTransportLabel(selectedDevice).split(' ')[0]}
                  </div>
                  <div className="text-[11px] text-slate-500 mt-0.5">
                    {renderTransportLabel(selectedDevice)}
                  </div>
                </div>
              </div>
            </div>

            {/* Announced Endpoints & Peer Connections */}
            <div className="p-3 border border-slate-200 rounded-md bg-white space-y-2 text-xs">
              <div className="font-semibold text-slate-800 flex items-center justify-between">
                <span>公告端点清单 (Endpoints) 与连通关系</span>
                <span className="font-mono text-[11px] text-slate-500">
                  互通节点: {selectedDevice.peers_connected.length} 台
                </span>
              </div>

              {selectedDevice.endpoints.length > 0 ? (
                <div className="space-y-1 font-mono text-[11px]">
                  {selectedDevice.endpoints.map((ep, i) => (
                    <div
                      key={i}
                      className="px-2 py-1 bg-slate-50 border border-slate-200 rounded text-slate-800 flex items-center justify-between"
                    >
                      <span>{ep}</span>
                      <span className="text-slate-400">
                        {selectedDevice.online ? 'UDP/QUIC' : 'Cached'}
                      </span>
                    </div>
                  ))}
                </div>
              ) : (
                <div className="p-2.5 bg-amber-50/70 border border-amber-200 rounded text-amber-900 text-xs leading-relaxed">
                  <div className="font-semibold flex items-center gap-1.5">
                    <AlertTriangle className="w-3.5 h-3.5 text-amber-600 shrink-0" />
                    <span>当前成员没有可拨端点（已属于网络名册）</span>
                  </div>
                  <p className="mt-1 text-[11px] text-amber-800">
                    {selectedDevice.diagnostic_reason}
                  </p>
                </div>
              )}

              <div className="pt-1 text-[11px] text-slate-600">
                <span className="text-slate-500">当前通信状态：</span>
                <span className="font-medium text-slate-800">
                  {selectedDevice.active_comm_summary}
                </span>
              </div>
            </div>

            {/* Nested Accordion: Deep Container Environment & Zero-Privilege Spec (Hidden from surface until clicked) */}
            <div className="border border-slate-200 rounded-md overflow-hidden bg-white">
              <button
                onClick={() => setShowConfigAccordion(!showConfigAccordion)}
                className="w-full px-3 py-2.5 bg-slate-50 hover:bg-slate-100 flex items-center justify-between text-xs font-semibold text-slate-800 transition-colors cursor-pointer"
              >
                <span className="flex items-center gap-1.5">
                  <Sliders className="w-3.5 h-3.5 text-sky-600" />
                  <span>展开嵌套容器底层配置与零特权环境参数</span>
                </span>
                {showConfigAccordion ? (
                  <ChevronUp className="w-4 h-4 text-slate-500" />
                ) : (
                  <ChevronDown className="w-4 h-4 text-slate-500" />
                )}
              </button>

              {showConfigAccordion && (
                <div className="p-3 space-y-2 text-xs border-t border-slate-200 font-mono bg-white">
                  <div className="grid grid-cols-2 gap-2">
                    <div>
                      <span className="text-slate-400 block text-[10px]">容器运行时</span>
                      <span className="text-slate-800">
                        {selectedDevice.container_config.runtime}
                      </span>
                    </div>
                    <div>
                      <span className="text-slate-400 block text-[10px]">镜像 / 客户端版本</span>
                      <span className="text-slate-800 truncate block">
                        {selectedDevice.container_config.image}
                      </span>
                    </div>
                    <div>
                      <span className="text-slate-400 block text-[10px]">进程权限 (零 root)</span>
                      <span className="text-emerald-700 font-semibold">
                        {selectedDevice.container_config.privileges}
                      </span>
                    </div>
                    <div>
                      <span className="text-slate-400 block text-[10px]">内核特权依赖</span>
                      <span className="text-emerald-700 font-semibold">
                        CAP_NET_ADMIN: 否 · TUN: 否
                      </span>
                    </div>
                    <div>
                      <span className="text-slate-400 block text-[10px]">入站端口策略</span>
                      <span className="text-slate-800">
                        {selectedDevice.container_config.inbound_ports}
                      </span>
                    </div>
                    <div>
                      <span className="text-slate-400 block text-[10px]">资源配额</span>
                      <span className="text-slate-800">
                        {selectedDevice.container_config.cpu_limit} /{' '}
                        {selectedDevice.container_config.memory_limit}
                      </span>
                    </div>
                  </div>
                  <div className="pt-1.5 border-t border-slate-100 text-[11px] text-slate-600">
                    工作目录: {selectedDevice.container_config.workdir}
                  </div>
                </div>
              )}
            </div>

            {/* Verified Userspace Operations Workbench (Message, File, RTT, Controlled Command) */}
            <div className="p-3 border border-slate-200 rounded-md bg-white space-y-3">
              <div className="flex items-center justify-between">
                <span className="text-xs font-semibold text-slate-800 flex items-center gap-1.5">
                  <ShieldCheck className="w-4 h-4 text-sky-600" />
                  <span>容器间直接操作 (已完成底层能力)</span>
                </span>
                <span className="text-[11px] font-mono text-slate-400">Userspace Data Plane</span>
              </div>

              {!selectedDevice.online ? (
                <div className="p-2.5 bg-slate-100 rounded text-xs text-slate-600">
                  当前容器处于离线状态（仍在名册中），实时通信操作已挂起。点击上方「恢复该容器上线」即可测试通信。
                </div>
              ) : (
                <>
                  {/* Quick Action Buttons */}
                  <div className="grid grid-cols-2 gap-2">
                    <button
                      disabled={actionLoading !== null}
                      onClick={() => executeDeviceAction(selectedDevice, 'ping')}
                      className="px-3 py-2 bg-slate-900 hover:bg-slate-800 text-white rounded text-xs font-medium flex items-center justify-center gap-1.5 transition-colors cursor-pointer"
                    >
                      <Activity className="w-3.5 h-3.5 text-sky-400" />
                      <span>测量 RTT 延迟</span>
                    </button>

                    <button
                      disabled={actionLoading !== null}
                      onClick={() => executeDeviceAction(selectedDevice, 'file_transfer')}
                      className="px-3 py-2 bg-sky-700 hover:bg-sky-800 text-white rounded text-xs font-medium flex items-center justify-center gap-1.5 transition-colors cursor-pointer"
                    >
                      <FileUp className="w-3.5 h-3.5" />
                      <span>传输构建产物 (256MB)</span>
                    </button>
                  </div>

                  {/* Send P2P Message Input */}
                  <div className="flex items-center gap-1.5">
                    <input
                      type="text"
                      value={msgInput}
                      onChange={(e) => setMsgInput(e.target.value)}
                      placeholder="输入发送给该容器的消息..."
                      className="flex-1 px-2.5 py-1.5 text-xs border border-slate-200 rounded font-mono focus:outline-none focus:border-sky-600"
                    />
                    <button
                      disabled={actionLoading !== null}
                      onClick={() => executeDeviceAction(selectedDevice, 'message')}
                      className="px-3 py-1.5 bg-slate-100 hover:bg-slate-200 text-slate-800 border border-slate-300 rounded text-xs font-medium flex items-center gap-1 shrink-0 cursor-pointer"
                    >
                      <Send className="w-3 h-3" />
                      <span>发消息</span>
                    </button>
                  </div>

                  {/* Controlled Whitelist Remote Command */}
                  <div className="flex items-center gap-1.5">
                    <select
                      value={selectedCmd}
                      onChange={(e) => setSelectedCmd(e.target.value)}
                      className="flex-1 px-2 py-1.5 text-xs border border-slate-200 rounded font-mono bg-white focus:outline-none focus:border-sky-600"
                    >
                      {selectedDevice.container_config.whitelist_cmds.map((cmd) => (
                        <option key={cmd} value={cmd}>
                          白名单命令: {cmd}
                        </option>
                      ))}
                    </select>
                    <button
                      disabled={actionLoading !== null}
                      onClick={() => executeDeviceAction(selectedDevice, 'exec_whitelist')}
                      className="px-3 py-1.5 bg-slate-100 hover:bg-slate-200 text-slate-800 border border-slate-300 rounded text-xs font-medium flex items-center gap-1 shrink-0 cursor-pointer"
                    >
                      <Terminal className="w-3 h-3" />
                      <span>受控执行</span>
                    </button>
                  </div>
                </>
              )}

              {/* Action Execution Output Box */}
              {actionOutput && (
                <div className="p-2.5 bg-slate-900 text-slate-100 rounded text-[11px] font-mono space-y-1">
                  <div className="flex items-center justify-between text-sky-400">
                    <span className="font-semibold">{actionOutput.title}</span>
                    <span className="text-[10px] text-slate-400">
                      {actionOutput.timestamp.slice(11, 19)}
                    </span>
                  </div>
                  <pre className="whitespace-pre-wrap text-slate-200 leading-relaxed overflow-x-auto">
                    {actionOutput.detail}
                  </pre>
                </div>
              )}
            </div>
          </div>
        )}
      </div>

      {/* On-Demand Collapsible Network Diagnostics Drawer (Chapter 8.5 Requirement) */}
      <div className="border-t border-slate-200 bg-slate-50">
        <button
          onClick={() => setShowDiagnosticsDrawer(!showDiagnosticsDrawer)}
          className="w-full px-4 py-2.5 flex items-center justify-between text-xs font-medium text-slate-700 hover:text-slate-900 cursor-pointer"
        >
          <span className="flex items-center gap-2">
            <span>网络诊断与同步状态抽屉 (按需展开)</span>
            {meshData.is_cached && (
              <span className="text-amber-700 font-mono font-semibold">▲ 缓存快照模式</span>
            )}
          </span>
          {showDiagnosticsDrawer ? (
            <ChevronDown className="w-4 h-4 text-slate-500" />
          ) : (
            <ChevronUp className="w-4 h-4 text-slate-500" />
          )}
        </button>

        {showDiagnosticsDrawer && (
          <div className="px-4 pb-3.5 pt-1 border-t border-slate-200/70 space-y-2 text-xs text-slate-600 font-mono">
            <div className="grid grid-cols-2 gap-2 bg-white p-2.5 rounded border border-slate-200">
              <div>
                <span className="text-slate-400 block text-[10px]">发现节点连接状态</span>
                <span
                  className={
                    meshData.discovery_status === 'connected'
                      ? 'text-emerald-700 font-semibold'
                      : 'text-amber-700 font-semibold'
                  }
                >
                  {meshData.discovery_status === 'connected'
                    ? '● 正常连接 (anchor-hk-01)'
                    : '▲ 发现节点失联 · 自动重连中'}
                </span>
              </div>
              <div>
                <span className="text-slate-400 block text-[10px]">当前视图数据来源</span>
                <span className="text-slate-800 font-semibold">
                  {meshData.is_cached ? '本地缓存快照 (Cached)' : '实时接口 (/api/mesh)'}
                </span>
              </div>
              <div>
                <span className="text-slate-400 block text-[10px]">快照生成时间</span>
                <span className="text-slate-800">
                  {meshData.generated_at.replace('T', ' ').slice(0, 19)}
                </span>
              </div>
              <div>
                <span className="text-slate-400 block text-[10px]">自动重试状态</span>
                <span className="text-slate-800">
                  {meshData.retry_in_seconds
                    ? `后台自动重试 (${meshData.retry_in_seconds}s)`
                    : '无需重试 (同步正常)'}
                </span>
              </div>
            </div>

            {selectedDevice && (
              <div className="p-2.5 bg-white rounded border border-slate-200 font-sans">
                <div className="font-semibold text-slate-800 mb-0.5">
                  当前选中节点诊断说明 ({selectedDevice.display_name})：
                </div>
                <p className="text-slate-600 leading-relaxed">{selectedDevice.diagnostic_reason}</p>
              </div>
            )}
          </div>
        )}
      </div>
    </aside>
  );
};
