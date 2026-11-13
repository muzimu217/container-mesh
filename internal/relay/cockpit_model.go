// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package relay

// The cockpit's presentation model.
//
// The accepted cockpit draws the network on a world map: zones (places),
// devices inside them, and a per-device "archetype" that decides whether
// the device is drawn as an open frame or under a glass dome.
//
// cmesh does not know where its devices are, and it does not know what runs
// inside them. So the model below is built from what the mesh actually
// knows, and says so where it has nothing:
//
//   - there is ONE zone — the deployment itself. The map places every device
//     in the same spot and the cockpit spreads them into a cluster, which is
//     the honest picture: this is one self-hosted network;
//   - a device's reachability decides its shape: a device that announced an
//     address is drawn open (something can dial it), a device without one is
//     drawn sealed (it can only be reached through another member);
//   - geography is reported as NOT REGISTERED rather than invented — a
//     fabricated city is worse than an empty field;
//   - the container fingerprint is reported as NOT COLLECTED. cmesh does not
//     profile the machine it runs on.

import (
	"fmt"
	"time"

	"github.com/muzimu217/container-mesh/internal/identity"
)

// CockpitZone is one place in the cockpit's map.
type CockpitZone struct {
	ZoneID        string     `json:"zone_id"`
	ZoneName      string     `json:"zone_name"`
	ZoneCode      string     `json:"zone_code"`
	Environment   string     `json:"environment_type"`
	LocationLabel string     `json:"location_label"`
	GeoLat        float64    `json:"geo_lat"`
	GeoLng        float64    `json:"geo_lng"`
	GeoCoordLabel string     `json:"geo_coord_label"`
	Description   string     `json:"description"`
	Position3D    [3]float64 `json:"position_3d"`
}

// CockpitMember is one device as the cockpit draws it.
type CockpitMember struct {
	DeviceID    string `json:"device_id"`
	DisplayName string `json:"display_name"`
	Alias       string `json:"alias,omitempty"`
	ZoneID      string `json:"zone_id"`
	ZoneName    string `json:"zone_name"`

	GeoLat        float64 `json:"geo_lat"`
	GeoLng        float64 `json:"geo_lng"`
	GeoCity       string  `json:"geo_city"`
	GeoCoordLabel string  `json:"geo_coord_label"`

	Role             string `json:"role"` // anchor | local | member
	IsAnchor         bool   `json:"is_anchor"`
	IsLocal          bool   `json:"is_local"`
	Archetype        string `json:"container_archetype"`
	HasPublicInbound bool   `json:"has_public_inbound"`
	NATTypeLabel     string `json:"nat_type_label"`

	InRoster             bool                   `json:"in_roster"`
	Online               bool                   `json:"online"`
	Endpoints            []string               `json:"endpoints"`
	LastSeen             string                 `json:"last_seen"`
	Transport            string                 `json:"transport"` // direct | relay | pending | unavailable
	PeersConnected       []string               `json:"peers_connected"`
	ActiveCommSummary    string                 `json:"active_comm_summary"`
	DiagnosticReason     string                 `json:"diagnostic_reason"`
	DiagnosticSuggestion string                 `json:"diagnostic_suggestion"`
	ContainerConfig      CockpitContainerConfig `json:"container_config"`
}

// CockpitContainerConfig is the device's runtime fingerprint.
type CockpitContainerConfig struct {
	Runtime         string   `json:"runtime"`
	Image           string   `json:"image"`
	Privileges      string   `json:"privileges"`
	CapNetAdmin     bool     `json:"cap_net_admin"`
	TUNDevice       bool     `json:"tun_device"`
	InboundPorts    string   `json:"inbound_ports"`
	UserSpaceSocket string   `json:"userspace_socket"`
	OSArch          string   `json:"OS_arch"`
	CPULimit        string   `json:"cpu_limit"`
	MemoryLimit     string   `json:"memory_limit"`
	Workdir         string   `json:"workdir"`
	WhitelistCmds   []string `json:"whitelist_cmds"`
}

// CockpitPayload is the whole cockpit response.
type CockpitPayload struct {
	AccountID       string          `json:"account_id"`
	NetworkName     string          `json:"network_name"`
	RosterVersion   uint64          `json:"roster_version"`
	GeneratedAt     string          `json:"generated_at"`
	DiscoveryStatus string          `json:"discovery_status"`
	IsCached        bool            `json:"is_cached"`
	LastSyncAt      string          `json:"last_sync_at"`
	RetryInSeconds  *int            `json:"retry_in_seconds"`
	Zones           []CockpitZone   `json:"zones"`
	Members         []CockpitMember `json:"members"`
}

// The single zone of a self-hosted deployment.
const (
	cockpitZoneID   = "zone_self_hosted"
	cockpitZoneName = "自托管私有网络 · 本部署"
	cockpitZoneCode = "ZONE-01 / SELF-HOSTED"
	// notRegistered is what cmesh says about a device's location, because
	// cmesh does not collect one.
	notRegistered = "未登记"
	// notCollected is what cmesh says about a device's runtime, because
	// cmesh does not profile one.
	notCollected = "未采集"
)

// buildCockpitPayload turns the live roster into the cockpit's view model.
func buildCockpitPayload(rows []PanelMember, accountID identity.DeviceID, rosterVersion uint64, generatedAt time.Time, connected bool, retrySeconds int) CockpitPayload {
	now := generatedAt.UTC()
	payload := CockpitPayload{
		AccountID:       string(accountID),
		NetworkName:     string(accountID),
		RosterVersion:   rosterVersion,
		GeneratedAt:     now.Format(time.RFC3339),
		DiscoveryStatus: "connected",
		IsCached:        false,
		LastSyncAt:      now.Format(time.RFC3339),
		RetryInSeconds:  nil,
		Zones: []CockpitZone{{
			ZoneID:        cockpitZoneID,
			ZoneName:      cockpitZoneName,
			ZoneCode:      cockpitZoneCode,
			Environment:   "自托管部署 (Self-Hosted Deployment)",
			LocationLabel: "本部署 · 成员地理位置未登记",
			GeoCoordLabel: "地理未登记 · cmesh 不采集位置信息",
			Description:   "本网络的所有成员都在同一个自托管部署中；成员之间的地理位置未被采集。",
			Position3D:    [3]float64{0, 0, 0},
		}},
		Members: make([]CockpitMember, 0, len(rows)),
	}
	if !connected {
		payload.DiscoveryStatus = "disconnected_cached"
		payload.IsCached = true
		payload.RetryInSeconds = &retrySeconds
	}
	for _, row := range rows {
		m := CockpitMember{
			DeviceID:    row.DeviceID.String(),
			DisplayName: row.Name,
			ZoneID:      cockpitZoneID,
			ZoneName:    cockpitZoneName,
			// Geography: not collected. The coordinates are a layout hint on
			// the map, not a claim about where the device is.
			GeoLat:         0,
			GeoLng:         0,
			GeoCity:        notRegistered,
			GeoCoordLabel:  "地理未登记 · cmesh 不采集位置信息",
			Role:           "member",
			IsAnchor:       row.IsAnchor,
			IsLocal:        row.IsLocal,
			InRoster:       true,
			Online:         row.Online,
			Endpoints:      row.Endpoints,
			LastSeen:       row.LastSeen,
			PeersConnected: []string{},
			ContainerConfig: CockpitContainerConfig{
				Runtime:         notCollected,
				Image:           notCollected,
				Privileges:      notCollected,
				CapNetAdmin:     false,
				TUNDevice:       false,
				InboundPorts:    notCollected,
				UserSpaceSocket: notCollected,
				OSArch:          notCollected,
				CPULimit:        notCollected,
				MemoryLimit:     notCollected,
				Workdir:         notCollected,
				WhitelistCmds:   []string{},
			},
		}
		if m.Endpoints == nil {
			m.Endpoints = []string{}
		}
		switch {
		case row.IsAnchor:
			m.Role = "anchor"
			m.Archetype = "public_anchor_tower"
			m.HasPublicInbound = true
			m.NATTypeLabel = "公网可达锚点 · 发现节点"
			m.ActiveCommSummary = "名册分发与端点协调"
		case len(row.Endpoints) > 0:
			m.Role = "member"
			if row.IsLocal {
				m.Role = "local"
			}
			m.Archetype = "nat_traversal_workstation"
			m.HasPublicInbound = false
			m.NATTypeLabel = "已公告直连端点 · 可被点对点直连"
			m.ActiveCommSummary = "直连可达"
		case row.Online:
			m.Archetype = "nat_restricted_sandbox"
			m.NATTypeLabel = "仅出站受限 · 无直连端点，经锚点中转"
			m.ActiveCommSummary = "经锚点中转可达"
		default:
			m.Archetype = "cgnat_archive_vault"
			m.NATTypeLabel = "当前离线 · 仍保留名册席位"
			m.ActiveCommSummary = "离线（仍在名册）"
		}
		switch {
		case !row.Online:
			m.Transport = "unavailable"
			m.DiagnosticReason = fmt.Sprintf("设备当前离线，最后在线 %s", row.LastSeenRel)
			m.DiagnosticSuggestion = "检查该设备是否仍在运行并已公告端点；离线设备保留名册席位，不会被除名。"
		case len(row.Endpoints) == 0:
			m.Transport = "relay"
			m.DiagnosticReason = "该设备没有公告任何直连端点，其他成员无法直接拨它。"
			m.DiagnosticSuggestion = "在该设备上以 --listen 启动以公告直连端点；否则流量需经锚点中转。"
		default:
			m.Transport = "direct"
			m.DiagnosticReason = fmt.Sprintf("该设备已公告 %d 个直连端点。", len(row.Endpoints))
			m.DiagnosticSuggestion = "可直接点对点直连；无需中转。"
		}
		payload.Members = append(payload.Members, m)
	}
	return payload
}
