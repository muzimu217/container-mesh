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

// The prototype's demo scenarios were removed: the product shows the real
// roster only. A dashboard that can paint fiction is a dashboard that lies.
