export type TransportMode = 'direct' | 'relay' | 'pending' | 'unavailable';

export type ScenarioKey =
  | 'normal'
  | 'partial_offline'
  | 'no_endpoint'
  | 'discovery_lost'
  | 'all_online';

export interface ContainerInternalConfig {
  runtime: string;
  image: string;
  privileges: string;
  cap_net_admin: boolean;
  tun_device: boolean;
  inbound_ports: string;
  userspace_socket: string;
  OS_arch: string;
  cpu_limit: string;
  memory_limit: string;
  workdir: string;
  whitelist_cmds: string[];
}

export type ContainerArchetype =
  | 'public_anchor_tower' // 公网自托管锚点塔（无封闭玻璃罩，开放式多环发射阵列）
  | 'nat_traversal_workstation' // 锥形穿透本机终端（半开放光学导流罩 + 双频打洞探针）
  | 'public_direct_compute' // 开放式直连算力刀片组（有公网/端口映射，无玻璃罩，裸露金属散热矩阵）
  | 'nat_restricted_sandbox' // 严苛 NAT 仅出站受限沙箱（双层密封全包覆防爆玻璃舱，无入站天线）
  | 'cgnat_archive_vault'; // 家庭 CGNAT 归档存储阵列（封闭式重型玻璃护罩 + 双盘位圆柱阵列）

export interface MeshMember {
  device_id: string;
  display_name: string;
  alias?: string;
  zone_id: string;
  zone_name: string;
  geo_lat: number;
  geo_lng: number;
  geo_city: string;
  geo_coord_label: string;
  role: 'anchor' | 'local' | 'member';
  is_anchor: boolean;
  is_local?: boolean;
  container_archetype: ContainerArchetype;
  has_public_inbound: boolean; // True = Open unshielded architecture (no glass dome); False = NAT/Restricted glass enclosure
  nat_type_label: string;
  in_roster: boolean; // 1. Membership
  online: boolean; // 2. Liveness
  endpoints: string[]; // 3. Announcement
  last_seen: string;
  transport: TransportMode;
  peers_connected: string[]; // IDs of peers currently communicating with
  active_comm_summary: string; // Human-readable communication status
  diagnostic_reason: string;
  diagnostic_suggestion: string;
  container_config: ContainerInternalConfig;
}

export interface MeshZone {
  zone_id: string;
  zone_name: string;
  zone_code: string;
  environment_type: string;
  location_label: string;
  geo_lat: number;
  geo_lng: number;
  geo_coord_label: string;
  description: string;
  position_3d: [number, number, number];
}

export interface MeshApiResponse {
  account_id: string;
  network_name: string;
  roster_version: number;
  generated_at: string;
  discovery_status: 'connected' | 'disconnected_cached';
  is_cached: boolean;
  last_sync_at: string;
  retry_in_seconds: number | null;
  zones: MeshZone[];
  members: MeshMember[];
}

const ZONES: MeshZone[] = [
  {
    zone_id: 'zone_anchor_hub',
    zone_name: '自托管锚点机房 · 中国香港',
    zone_code: 'ZONE-01 / ANCHOR-HKG',
    environment_type: '自托管云主机 (Self-Hosted VPS)',
    location_label: 'HKG-Zone-A · 唯一公网可达锚点',
    geo_lat: 22.32,
    geo_lng: 114.17,
    geo_coord_label: '22.32°N, 114.17°E · 香港',
    description: '负责全网名册分发、端到端身份协调与受限容器的中继兜底，由用户完全自主控制。',
    position_3d: [0, 0, -3.2],
  },
  {
    zone_id: 'zone_cloud_prod',
    zone_name: '欧洲云端与本机集群 · 法兰克福/苏黎世',
    zone_code: 'ZONE-02 / EURO-CORE',
    environment_type: 'Docker 容器与开发工作站',
    location_label: 'FRA/ZRH · 跨国 UDP 打洞直连区',
    geo_lat: 49.8,
    geo_lng: 11.5,
    geo_coord_label: '50.11°N, 8.68°E · 中欧核心区',
    description: '承载本机开发客户端与远端 GPU 推理容器，无需开放安全组入站即可建立点对点直连。',
    position_3d: [-4.0, 0, 2.0],
  },
  {
    zone_id: 'zone_edge_sandbox',
    zone_name: '亚太受限沙箱与家庭内网区 · 东京/新加坡',
    zone_code: 'ZONE-03 / APAC-EDGE',
    environment_type: '无 root 受限容器 & 家庭 NAS',
    location_label: 'NRT/SIN · 零特权仅出站与 CGNAT 环境',
    geo_lat: 18.5,
    geo_lng: 122.0,
    geo_coord_label: '35.68°N / 1.35°N · 亚太边缘',
    description: '包含无 CAP_NET_ADMIN 的东京 CI 构建沙箱以及处于运营商 CGNAT 下的新加坡家庭存储节点。',
    position_3d: [4.0, 0, 2.0],
  },
];

const BASE_MEMBERS: MeshMember[] = [
  {
    device_id: 'dev_01_anchor_hk',
    display_name: 'anchor-hk-01',
    alias: '香港自托管发现锚点',
    zone_id: 'zone_anchor_hub',
    zone_name: '自托管锚点机房 · 中国香港',
    geo_lat: 22.32,
    geo_lng: 114.17,
    geo_city: '中国香港 (HKG)',
    geo_coord_label: '22.32°N, 114.17°E',
    role: 'anchor',
    is_anchor: true,
    container_archetype: 'public_anchor_tower',
    has_public_inbound: true,
    nat_type_label: '公网直暴露 (无玻璃罩 · 开放式信标塔)',
    in_roster: true,
    online: true,
    endpoints: ['203.0.113.10:41820', '[2001:db8:hk::10]:41820'],
    last_seen: '2026-11-11T16:11:20Z',
    transport: 'direct',
    peers_connected: ['dev_02_local_ws', 'dev_03_gpu_fra', 'dev_04_ci_sandbox'],
    active_comm_summary: '名册分发 v42 正常 · 维持 3 条跨洲控制信道',
    diagnostic_reason: '自托管发现与中继锚点运行正常，公网 UDP/TCP 端点可拨通。',
    diagnostic_suggestion: '当前作为网络信任锚点，无需额外操作。',
    container_config: {
      runtime: 'oci-runc (Docker 26.1)',
      image: 'ghcr.io/cmesh-net/anchor:v0.9.4',
      privileges: 'uid=1000 (非 root 普通用户)',
      cap_net_admin: false,
      tun_device: false,
      inbound_ports: 'UDP/41820 (仅锚点开启 --listen)',
      userspace_socket: 'QUIC / Userspace Framed Stream',
      OS_arch: 'linux/amd64 (Debian 12 Bookworm)',
      cpu_limit: '2.0 vCPU',
      memory_limit: '2048 MB',
      workdir: '/var/lib/cmesh-anchor',
      whitelist_cmds: ['cmesh status --json', 'uname -a', 'df -h /workspace', 'id && capsh --print'],
    },
  },
  {
    device_id: 'dev_02_local_ws',
    display_name: 'macbook-studio-local',
    alias: '当前本机客户端 (苏黎世)',
    zone_id: 'zone_cloud_prod',
    zone_name: '欧洲云端与本机集群 · 法兰克福/苏黎世',
    geo_lat: 47.37,
    geo_lng: 6.54,
    geo_city: '瑞士苏黎世 (ZRH)',
    geo_coord_label: '47.37°N, 6.54°E',
    role: 'local',
    is_anchor: false,
    is_local: true,
    container_archetype: 'nat_traversal_workstation',
    has_public_inbound: false,
    nat_type_label: '锥形 NAT 穿透终端 (半开放光学折流环)',
    in_roster: true,
    online: true,
    endpoints: ['198.51.100.44:51902', '192.168.31.105:51902'],
    last_seen: '2026-11-11T16:11:24Z',
    transport: 'direct',
    peers_connected: ['dev_01_anchor_hk', 'dev_03_gpu_fra', 'dev_04_ci_sandbox'],
    active_comm_summary: '本机控制台在线 · 与法兰克福 gpu-trainer-02 点对点直连互通',
    diagnostic_reason: '本机用户态守护进程 cmeshd 运行正常，已公告局域网与 NAT 映射端点。',
    diagnostic_suggestion: '可直接向在线节点发起消息、文件传输或白名单命令。',
    container_config: {
      runtime: 'Native Desktop Daemon (cmesh-app)',
      image: 'cmesh-desktop-client v0.9.4',
      privileges: 'Standard User (无系统管理员/root 权限)',
      cap_net_admin: false,
      tun_device: false,
      inbound_ports: '无公网入站 (STUN/NAT 穿透直连)',
      userspace_socket: 'Local IPC + Userspace P2P Socket',
      OS_arch: 'darwin/arm64 (or windows/amd64)',
      cpu_limit: 'Unrestricted (Host User)',
      memory_limit: '128 MB RSS',
      workdir: '~/Library/Application Support/cmesh',
      whitelist_cmds: ['cmesh status --json', 'uname -a', 'df -h /workspace', 'id && capsh --print'],
    },
  },
  {
    device_id: 'dev_03_gpu_fra',
    display_name: 'gpu-trainer-02',
    alias: '法兰克福推理容器 A100',
    zone_id: 'zone_cloud_prod',
    zone_name: '欧洲云端与本机集群 · 法兰克福/苏黎世',
    geo_lat: 52.52,
    geo_lng: 16.40,
    geo_city: '德国法兰克福 (FRA)',
    geo_coord_label: '50.11°N, 8.68°E',
    role: 'member',
    is_anchor: false,
    container_archetype: 'public_direct_compute',
    has_public_inbound: true,
    nat_type_label: '公网端口直达 (无玻璃罩 · 裸露四联装刀片矩阵)',
    in_roster: true,
    online: true,
    endpoints: ['198.51.100.89:41820', '10.244.1.18:41820'],
    last_seen: '2026-11-11T16:11:22Z',
    transport: 'direct',
    peers_connected: ['dev_01_anchor_hk', 'dev_02_local_ws'],
    active_comm_summary: 'P2P 直连活跃 · 正在与苏黎世本机同步模型权重分片',
    diagnostic_reason: '已公告可用端点，与本机通过用户态 UDP 打洞建立点对点直连。',
    diagnostic_suggestion: '点对点直连已建立，数据面不经过香港锚点服务器。',
    container_config: {
      runtime: 'containerd (Kubernetes Pod · 无特权)',
      image: 'pytorch/pytorch:2.4-cuda12.1-cmesh',
      privileges: 'uid=1001(researcher) · AllowPrivilegeEscalation=false',
      cap_net_admin: false,
      tun_device: false,
      inbound_ports: 'UDP/41820 (HostPort 映射)',
      userspace_socket: 'Userspace Direct P2P (ChaCha20-Poly1305)',
      OS_arch: 'linux/amd64 (Ubuntu 22.04)',
      cpu_limit: '8.0 vCPU',
      memory_limit: '32768 MB',
      workdir: '/workspace/checkpoints',
      whitelist_cmds: ['cmesh status --json', 'uname -a', 'df -h /workspace', 'id && capsh --print'],
    },
  },
  {
    device_id: 'dev_04_ci_sandbox',
    display_name: 'ci-runner-ephemeral',
    alias: '东京受限构建沙箱 (仅出站)',
    zone_id: 'zone_edge_sandbox',
    zone_name: '亚太受限沙箱与家庭内网区 · 东京/新加坡',
    geo_lat: 35.68,
    geo_lng: 139.76,
    geo_city: '日本东京 (NRT)',
    geo_coord_label: '35.68°N, 139.76°E',
    role: 'member',
    is_anchor: false,
    container_archetype: 'nat_restricted_sandbox',
    has_public_inbound: false,
    nat_type_label: '严苛 NAT 受限沙箱 (全封闭光学隔离玻璃舱 · 无天线)',
    in_roster: true,
    online: true,
    endpoints: [],
    last_seen: '2026-11-11T16:11:19Z',
    transport: 'pending',
    peers_connected: ['dev_01_anchor_hk'],
    active_comm_summary: '已加入名册 · 当前无公告端点（仅从东京出站连接香港锚点）',
    diagnostic_reason: '该容器已属于网络且心跳在线，但当前未公告任何可被其他设备拨号的端点（endpoints 为空）。可能未开启 --listen 参数，或刚获批尚未完成首次端点公告。',
    diagnostic_suggestion: '若该沙箱仅主动拉取任务无需被拨入，可保持现状；若需被其他节点主动连接，请使用 cmesh up --listen 启动或启用锚点受控中继，也可稍候重试刷新。',
    container_config: {
      runtime: 'gVisor runsc (Serverless 强隔离沙箱)',
      image: 'ghcr.io/cmesh-net/ci-agent:alpine',
      privileges: 'uid=65534(nobody) · Zero Capabilities',
      cap_net_admin: false,
      tun_device: false,
      inbound_ports: '全部封禁 (Strict Egress-Only NAT)',
      userspace_socket: 'Outbound-Only Userspace Session',
      OS_arch: 'linux/amd64 (Alpine 3.20)',
      cpu_limit: '1.0 vCPU',
      memory_limit: '1024 MB',
      workdir: '/workspace/build-artifacts',
      whitelist_cmds: ['cmesh status --json', 'uname -a', 'df -h /workspace', 'id && capsh --print'],
    },
  },
  {
    device_id: 'dev_05_home_nas',
    display_name: 'home-nas-vault',
    alias: '新加坡家庭私有云归档节点',
    zone_id: 'zone_edge_sandbox',
    zone_name: '亚太受限沙箱与家庭内网区 · 东京/新加坡',
    geo_lat: 1.35,
    geo_lng: 103.82,
    geo_city: '新加坡 (SIN)',
    geo_coord_label: '1.35°N, 103.82°E',
    role: 'member',
    is_anchor: false,
    container_archetype: 'cgnat_archive_vault',
    has_public_inbound: false,
    nat_type_label: '运营商 CGNAT 内网 (封闭式玻璃护罩 + 双盘位归档仓)',
    in_roster: true,
    online: false,
    endpoints: ['100.64.0.88:41820 (历史缓存端点)'],
    last_seen: '2026-11-11T14:35:10Z',
    transport: 'unavailable',
    peers_connected: [],
    active_comm_summary: '当前离线（仍在网络名册中 · 1小时36分前在线）',
    diagnostic_reason: '设备属于当前私有网络名册，但最近心跳停留于 14:35:10Z，当前不可达。请注意：离线不等于被移出网络，也不属于安全故障。',
    diagnostic_suggestion: '请检查家庭宽带电源或容器进程状态；设备重新上线后将自动恢复连通，无需重新审批入网。',
    container_config: {
      runtime: 'Podman Rootless (非特权守护进程)',
      image: 'ghcr.io/cmesh-net/node:v0.9.4-arm64',
      privileges: 'uid=1000(nas-admin) Rootless',
      cap_net_admin: false,
      tun_device: false,
      inbound_ports: 'CGNAT 无公网 IP (依赖锚点协调)',
      userspace_socket: 'Inactive (Last mode: Relay)',
      OS_arch: 'linux/arm64 (Debian 12)',
      cpu_limit: '4.0 Cores',
      memory_limit: '8192 MB',
      workdir: '/mnt/raid1/cmesh-vault',
      whitelist_cmds: ['cmesh status --json', 'uname -a', 'df -h /workspace', 'id && capsh --print'],
    },
  },
];

export function getMeshDataByScenario(scenario: string): MeshApiResponse {
  const nowIso = '2026-11-11T16:11:24Z';
  const membersCopy: MeshMember[] = JSON.parse(JSON.stringify(BASE_MEMBERS));

  switch (scenario) {
    case 'partial_offline': {
      // Simulate gpu-trainer-02 also going offline cleanly without red panic
      const gpu = membersCopy.find((m) => m.device_id === 'dev_03_gpu_fra');
      if (gpu) {
        gpu.online = false;
        gpu.transport = 'unavailable';
        gpu.peers_connected = [];
        gpu.last_seen = '2026-11-11T16:02:05Z';
        gpu.active_comm_summary = '当前离线（保留在册 · 9分钟前完成训练任务后休眠）';
        gpu.diagnostic_reason = '容器训练脚本执行完毕后正常停止，心跳超时转为离线。仍在名册 v42 中保留身份。';
      }
      const local = membersCopy.find((m) => m.device_id === 'dev_02_local_ws');
      if (local) {
        local.peers_connected = ['dev_01_anchor_hk', 'dev_04_ci_sandbox'];
        local.active_comm_summary = '本机在线 · 与香港锚点保持直连';
      }
      return {
        account_id: 'acct_9f82c104',
        network_name: 'prod-container-mesh',
        roster_version: 42,
        generated_at: nowIso,
        discovery_status: 'connected',
        is_cached: false,
        last_sync_at: nowIso,
        retry_in_seconds: null,
        zones: ZONES,
        members: membersCopy,
      };
    }

    case 'no_endpoint': {
      // Highlight multiple members approved in roster or online via relay/no-endpoint
      const nas = membersCopy.find((m) => m.device_id === 'dev_05_home_nas');
      if (nas) {
        nas.online = true;
        nas.endpoints = [];
        nas.transport = 'relay';
        nas.last_seen = '2026-11-11T16:11:21Z';
        nas.peers_connected = ['dev_01_anchor_hk', 'dev_02_local_ws'];
        nas.active_comm_summary = '在线可达 · 无公网入站端点，正通过香港锚点受控中继 (Relay)';
        nas.diagnostic_reason = '家庭宽带处于运营商 CGNAT 下，无法直接暴露入站端口，已自动回退至香港自托管锚点进行加密中继。';
      }
      return {
        account_id: 'acct_9f82c104',
        network_name: 'prod-container-mesh',
        roster_version: 43,
        generated_at: nowIso,
        discovery_status: 'connected',
        is_cached: false,
        last_sync_at: nowIso,
        retry_in_seconds: null,
        zones: ZONES,
        members: membersCopy,
      };
    }

    case 'discovery_lost': {
      // Discovery node unreachable -> warning banner, cached snapshot retained, auto-retry active
      const anchor = membersCopy.find((m) => m.device_id === 'dev_01_anchor_hk');
      if (anchor) {
        anchor.active_comm_summary = '发现节点轮询超时 · 当前展示 16:08:00Z 缓存名册快照';
        anchor.diagnostic_reason = '当前客户端无法连接香港发现节点获取最新名册（可能因跨境链路抖动或锚点重启），界面已冻结在最后一次有效快照 v42。';
        anchor.diagnostic_suggestion = '客户端正在后台自动指数退避重连（无需手动刷新页面），现有的点对点直连会话不受影响。';
      }
      return {
        account_id: 'acct_9f82c104',
        network_name: 'prod-container-mesh',
        roster_version: 42,
        generated_at: '2026-11-11T16:08:00Z',
        discovery_status: 'disconnected_cached',
        is_cached: true,
        last_sync_at: '2026-11-11T16:08:00Z',
        retry_in_seconds: 8,
        zones: ZONES,
        members: membersCopy,
      };
    }

    case 'all_online': {
      const ci = membersCopy.find((m) => m.device_id === 'dev_04_ci_sandbox');
      if (ci) {
        ci.endpoints = ['10.128.0.42:41820 (Relay Bound)'];
        ci.transport = 'relay';
        ci.peers_connected = ['dev_01_anchor_hk', 'dev_02_local_ws'];
        ci.active_comm_summary = '在线可达 · 经香港锚点中继与本机传输构建产物';
      }
      const nas = membersCopy.find((m) => m.device_id === 'dev_05_home_nas');
      if (nas) {
        nas.online = true;
        nas.endpoints = ['203.0.113.201:41820'];
        nas.transport = 'direct';
        nas.last_seen = nowIso;
        nas.peers_connected = ['dev_01_anchor_hk', 'dev_02_local_ws', 'dev_03_gpu_fra'];
        nas.active_comm_summary = '在线可达 · 点对点直连归档通道已建立';
      }
      return {
        account_id: 'acct_9f82c104',
        network_name: 'prod-container-mesh',
        roster_version: 44,
        generated_at: nowIso,
        discovery_status: 'connected',
        is_cached: false,
        last_sync_at: nowIso,
        retry_in_seconds: null,
        zones: ZONES,
        members: membersCopy,
      };
    }

    case 'normal':
    default:
      return {
        account_id: 'acct_9f82c104',
        network_name: 'prod-container-mesh',
        roster_version: 42,
        generated_at: nowIso,
        discovery_status: 'connected',
        is_cached: false,
        last_sync_at: nowIso,
        retry_in_seconds: null,
        zones: ZONES,
        members: membersCopy,
      };
  }
}
