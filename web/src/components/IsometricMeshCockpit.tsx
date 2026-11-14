import React, { useEffect, useMemo, useRef, useState } from 'react';
import * as THREE from 'three';
import { OrbitControls } from 'three/examples/jsm/controls/OrbitControls.js';
import { RoomEnvironment } from 'three/examples/jsm/environments/RoomEnvironment.js';
import { MeshMember, MeshZone } from '../data/meshSnapshots';
import {
  Globe,
  Layers,
  Maximize2,
  Minimize2,
  RotateCcw,
  Eye,
  EyeOff,
  ChevronRight,
  Box,
  Server,
  Radio,
  ArrowLeft,
  Compass,
  MapPin,
  Crosshair,
} from 'lucide-react';

interface IsometricMeshCockpitProps {
  zones: MeshZone[];
  members: MeshMember[];
  isCached: boolean;
  selectedZoneId: string | null;
  selectedDeviceId: string | null;
  onSelectZone: (zoneId: string | null) => void;
  onSelectDevice: (deviceId: string | null) => void;
  activeCommEvent: { fromId: string; toId: string; label: string } | null;
  ambientIntensity?: number;
  onChangeAmbientIntensity?: (val: number) => void;
  resolvedDark?: boolean;
}

interface ProjectedLabel {
  id: string;
  type: 'zone' | 'device';
  x: number;
  y: number;
  visible: boolean;
}

const GLOBE_RADIUS = 4.65;
// Compact micro-instrument scale on the globe so nodes never look oversized or block neighbors
const GLOBE_DEVICE_BASE_SCALE = 0.22;
const GLOBE_DEVICE_FOCUSED_SCALE = 0.29;
const LAB_DEVICE_SCALE = 0.88;
// Camera distance threshold in Globe mode: above this distance, same-city multi-device clusters collapse into a Regional Cluster Ring
const CLUSTER_ZOOM_DISTANCE_THRESHOLD = 11.4;

// Pre-allocated math helpers outside render loop (Zero GC)
const _projVec = new THREE.Vector3();
const _camToNode = new THREE.Vector3();
const _upVec = new THREE.Vector3(0, 1, 0);
const _identityQuat = new THREE.Quaternion();
const _targetCameraPos = new THREE.Vector3();
const _targetLookAt = new THREE.Vector3(0, 0, 0);
const _currentLookAt = new THREE.Vector3(0, 0, 0);
const _tangentLookVec = new THREE.Vector3();
const _raycaster = new THREE.Raycaster();
const _pointerNdc = new THREE.Vector2();

function getLabDeviceLocalOffset(deviceId: string, indexInZone: number, totalInZone: number): [number, number, number] {
  if (totalInZone <= 1) return [0, 0.22, 0];
  if (totalInZone === 2) {
    return indexInZone === 0 ? [-1.05, 0.22, -0.4] : [1.05, 0.22, 0.4];
  }
  // For 3+ nodes in the same zone (e.g. Hong Kong multi-node cluster demo), arrange in a clean ring on the slab
  const angle = (indexInZone / totalInZone) * Math.PI * 2 - Math.PI / 2;
  return [Math.cos(angle) * 1.15, 0.22, Math.sin(angle) * 0.85];
}

/**
 * Converts geographic (lat, lng) in degrees to 3D Cartesian coordinates on a sphere of given radius.
 */
function latLngToVector3(
  lat: number,
  lng: number,
  radius: number,
  target: THREE.Vector3 = new THREE.Vector3()
): THREE.Vector3 {
  const phi = THREE.MathUtils.degToRad(90 - lat);
  const theta = THREE.MathUtils.degToRad(lng + 180);
  const sinPhi = Math.sin(phi);

  target.x = -radius * sinPhi * Math.cos(theta);
  target.y = radius * Math.cos(phi);
  target.z = radius * sinPhi * Math.sin(theta);
  return target;
}

/**
 * Computes non-overlapping satellite coordinates when multiple servers are located in the SAME city
 * (e.g. 3 servers in Hong Kong). Distributes co-located nodes along a micro-ring around the city center
 * so they never occlude each other on the 3D globe!
 */
function computeSameCitySpreadLatLng(
  member: MeshMember,
  allMembers: MeshMember[],
  spreadRadiusDeg: number
): { lat: number; lng: number; isClustered: boolean; clusterIndex: number; clusterTotal: number } {
  // Find all nodes within 2.5 degrees of this node (same city / metropolitan cluster)
  const coLocated = allMembers.filter(
    (other) =>
      Math.hypot(other.geo_lat - member.geo_lat, other.geo_lng - member.geo_lng) < 2.5
  );

  if (coLocated.length <= 1) {
    return {
      lat: member.geo_lat,
      lng: member.geo_lng,
      isClustered: false,
      clusterIndex: 0,
      clusterTotal: 1,
    };
  }

  const idx = coLocated.findIndex((m) => m.device_id === member.device_id);
  const total = coLocated.length;
  // Distribute evenly around a micro-constellation ring around the city's true (lat, lng)
  const angle = (idx / total) * Math.PI * 2 - Math.PI / 2;
  // Account for longitude compression at higher latitudes
  const cosLat = Math.max(0.35, Math.cos(THREE.MathUtils.degToRad(member.geo_lat)));
  const offsetLat = Math.sin(angle) * spreadRadiusDeg;
  const offsetLng = (Math.cos(angle) * spreadRadiusDeg) / cosLat;

  return {
    lat: member.geo_lat + offsetLat,
    lng: member.geo_lng + offsetLng,
    isClustered: true,
    clusterIndex: idx,
    clusterTotal: total,
  };
}

/**
 * Resolves 2D screen-space bounding-box collisions between projected labels so co-located city tags
 * never stack on top of each other at any camera angle.
 */
function resolveScreenLabelOverlaps(labels: Record<string, ProjectedLabel>, rectW: number, rectH: number) {
  const visibleDeviceLabels = Object.values(labels).filter(
    (l) => l.visible && l.type === 'device'
  );

  const minDx = 108;
  const minDy = 28;

  for (let pass = 0; pass < 3; pass++) {
    for (let i = 0; i < visibleDeviceLabels.length; i++) {
      for (let j = i + 1; j < visibleDeviceLabels.length; j++) {
        const a = visibleDeviceLabels[i];
        const b = visibleDeviceLabels[j];
        const dx = b.x - a.x;
        const dy = b.y - a.y;

        if (Math.abs(dx) < minDx && Math.abs(dy) < minDy) {
          const pushY = (minDy - Math.abs(dy)) * 0.55 + 2;
          if (dy >= 0) {
            a.y = Math.max(28, a.y - pushY);
            b.y = Math.min(rectH - 28, b.y + pushY);
          } else {
            a.y = Math.min(rectH - 28, a.y + pushY);
            b.y = Math.max(28, b.y - pushY);
          }
          const pushX = (minDx - Math.abs(dx)) * 0.2;
          if (dx >= 0) {
            a.x = Math.max(40, a.x - pushX);
            b.x = Math.min(rectW - 40, b.x + pushX);
          } else {
            a.x = Math.min(rectW - 40, a.x + pushX);
            b.x = Math.max(40, b.x - pushX);
          }
        }
      }
    }
  }
}

/**
 * Generates an elevated 3D great-circle curve between two (lat, lng) points on the globe.
 */
function createGeodesicArcCurve(
  lat1: number,
  lng1: number,
  lat2: number,
  lng2: number,
  baseRadius: number,
  peakAltitude: number
): THREE.CatmullRomCurve3 {
  const startVec = latLngToVector3(lat1, lng1, 1).normalize();
  const endVec = latLngToVector3(lat2, lng2, 1).normalize();
  const omega = Math.acos(THREE.MathUtils.clamp(startVec.dot(endVec), -1, 1));
  const sinOmega = Math.sin(omega);

  const segments = 44;
  const points: THREE.Vector3[] = [];

  for (let i = 0; i <= segments; i++) {
    const t = i / segments;
    const v = new THREE.Vector3();
    if (sinOmega < 0.0001) {
      v.lerpVectors(startVec, endVec, t).normalize();
    } else {
      const w1 = Math.sin((1 - t) * omega) / sinOmega;
      const w2 = Math.sin(t * omega) / sinOmega;
      v.copy(startVec).multiplyScalar(w1).addScaledVector(endVec, w2).normalize();
    }
    const altitude = baseRadius + Math.sin(t * Math.PI) * peakAltitude;
    v.multiplyScalar(altitude);
    points.push(v);
  }

  return new THREE.CatmullRomCurve3(points);
}

/**
 * Generates a repeating directional stream gradient texture for semi-transparent geodesic arc paths.
 * Encodes flow direction (tail -> bright head) and supports smooth UV offset scrolling.
 */
function createDirectionalArcFlowTexture(): THREE.CanvasTexture {
  const canvas = document.createElement('canvas');
  canvas.width = 256;
  canvas.height = 32;
  const ctx = canvas.getContext('2d')!;

  ctx.clearRect(0, 0, 256, 32);

  // Translucent baseline glow along the entire arc path
  ctx.fillStyle = 'rgba(255, 255, 255, 0.22)';
  ctx.fillRect(0, 0, 256, 32);

  // Two directional comet pulses per UV repeat (ramping from tail 0.0 -> bright head 1.0)
  const drawCometPulse = (startX: number, width: number) => {
    const grad = ctx.createLinearGradient(startX, 0, startX + width, 0);
    grad.addColorStop(0.0, 'rgba(255, 255, 255, 0.0)');
    grad.addColorStop(0.25, 'rgba(255, 255, 255, 0.28)');
    grad.addColorStop(0.75, 'rgba(255, 255, 255, 0.78)');
    grad.addColorStop(0.95, 'rgba(255, 255, 255, 1.0)');
    grad.addColorStop(1.0, 'rgba(255, 255, 255, 0.15)');
    ctx.fillStyle = grad;
    ctx.fillRect(startX, 0, width, 32);
  };

  drawCometPulse(12, 96);
  drawCometPulse(140, 96);

  const tex = new THREE.CanvasTexture(canvas);
  tex.wrapS = THREE.RepeatWrapping;
  tex.wrapT = THREE.ClampToEdgeWrapping;
  tex.repeat.set(2.5, 1);
  tex.needsUpdate = true;
  return tex;
}

/**
 * Procedurally renders a 2048x1024 high-resolution Holographic Scientific World Map texture.
 */
function createHolographicEarthTexture(
  maxAnisotropy: number,
  members: MeshMember[]
): THREE.CanvasTexture {
  const canvas = document.createElement('canvas');
  canvas.width = 2048;
  canvas.height = 1024;
  const ctx = canvas.getContext('2d')!;

  const W = canvas.width;
  const H = canvas.height;

  const toX = (lng: number) => ((lng + 180) / 360) * W;
  const toY = (lat: number) => ((90 - lat) / 180) * H;

  const oceanGrad = ctx.createLinearGradient(0, 0, 0, H);
  oceanGrad.addColorStop(0, '#d8e8f8');
  oceanGrad.addColorStop(0.2, '#e4effa');
  oceanGrad.addColorStop(0.5, '#edf4fc');
  oceanGrad.addColorStop(0.8, '#e4effa');
  oceanGrad.addColorStop(1, '#d8e8f8');
  ctx.fillStyle = oceanGrad;
  ctx.fillRect(0, 0, W, H);

  ctx.strokeStyle = 'rgba(14, 165, 233, 0.09)';
  ctx.lineWidth = 1;
  for (let lng = -180; lng <= 180; lng += 5) {
    const x = toX(lng);
    ctx.beginPath();
    ctx.moveTo(x, 0);
    ctx.lineTo(x, H);
    ctx.stroke();
  }
  for (let lat = -85; lat <= 85; lat += 5) {
    const y = toY(lat);
    ctx.beginPath();
    ctx.moveTo(0, y);
    ctx.lineTo(W, y);
    ctx.stroke();
  }

  const continents: Array<Array<[number, number]>> = [
    [
      [36, -9], [43, -9], [48, -4], [54, 8], [60, 5], [70, 20], [71, 28],
      [68, 42], [70, 60], [73, 80], [76, 105], [72, 130], [66, 170], [60, 162],
      [52, 141], [42, 132], [38, 126], [35, 120], [31, 122], [22, 114],
      [18, 109], [10, 105], [2, 104], [14, 98], [22, 92], [16, 82], [8, 77],
      [22, 69], [25, 62], [26, 56], [14, 48], [13, 43], [28, 34], [31, 32],
      [36, 36], [41, 29], [38, 23], [40, 18], [44, 12], [38, 15], [43, 5],
      [36, -5],
    ],
    [
      [50, -5], [58, -6], [58, -2], [51, 1], [50, -5],
    ],
    [
      [31, 130], [35, 136], [36, 140], [41, 141], [44, 145], [40, 140],
      [34, 134], [31, 130],
    ],
    [
      [-6, 105], [-8, 115], [-5, 120], [1, 118], [5, 115], [1, 109], [-6, 105],
    ],
    [
      [-2, 132], [-9, 148], [-5, 146], [-1, 136], [-2, 132],
    ],
    [
      [35, -6], [37, 10], [32, 32], [12, 44], [11, 51], [-2, 41], [-15, 40],
      [-26, 33], [-34, 25], [-34, 18], [-18, 12], [-5, 12], [4, 9], [5, -4],
      [11, -16], [15, -17], [26, -14], [35, -6],
    ],
    [
      [-13, 49], [-25, 47], [-24, 44], [-15, 46], [-13, 49],
    ],
    [
      [-12, 131], [-12, 142], [-20, 149], [-28, 153], [-38, 146], [-35, 136],
      [-32, 128], [-34, 115], [-22, 114], [-15, 124], [-12, 131],
    ],
    [
      [-35, 173], [-46, 167], [-44, 172], [-37, 178], [-35, 173],
    ],
    [
      [70, -160], [71, -130], [68, -90], [62, -75], [52, -56], [44, -64],
      [40, -74], [30, -81], [25, -80], [28, -96], [20, -97], [18, -104],
      [24, -110], [32, -117], [40, -124], [49, -125], [58, -136], [61, -150],
      [60, -165], [70, -160],
    ],
    [
      [60, -44], [70, -22], [82, -30], [78, -68], [66, -54], [60, -44],
    ],
    [
      [11, -74], [10, -62], [4, -52], [-1, -48], [-6, -35], [-16, -39],
      [-23, -43], [-34, -53], [-46, -66], [-55, -68], [-50, -75], [-33, -72],
      [-18, -71], [-5, -81], [5, -77], [11, -74],
    ],
    [
      [-68, -180], [-66, -120], [-64, -60], [-68, 0], [-66, 70], [-65, 140],
      [-68, 180], [-88, 180], [-88, -180],
    ],
  ];

  ctx.save();
  ctx.beginPath();
  continents.forEach((poly) => {
    poly.forEach(([lat, lng], idx) => {
      const px = toX(lng);
      const py = toY(lat);
      if (idx === 0) ctx.moveTo(px, py);
      else ctx.lineTo(px, py);
    });
    ctx.closePath();
  });

  ctx.fillStyle = 'rgba(255, 255, 255, 0.95)';
  ctx.fill();

  ctx.clip();
  ctx.fillStyle = 'rgba(2, 132, 199, 0.30)';
  const dotSpacing = 10;
  for (let x = 4; x < W; x += dotSpacing) {
    for (let y = 4; y < H; y += dotSpacing) {
      ctx.fillRect(x - 1, y - 1, 2.3, 2.3);
    }
  }
  ctx.restore();

  ctx.strokeStyle = 'rgba(2, 132, 199, 0.6)';
  ctx.lineWidth = 2.2;
  continents.forEach((poly) => {
    ctx.beginPath();
    poly.forEach(([lat, lng], idx) => {
      const px = toX(lng);
      const py = toY(lat);
      if (idx === 0) ctx.moveTo(px, py);
      else ctx.lineTo(px, py);
    });
    ctx.closePath();
    ctx.stroke();
  });

  ctx.strokeStyle = 'rgba(2, 132, 199, 0.24)';
  ctx.lineWidth = 1.2;
  for (let lng = -180; lng <= 180; lng += 15) {
    const x = toX(lng);
    ctx.beginPath();
    ctx.moveTo(x, 0);
    ctx.lineTo(x, H);
    ctx.stroke();
  }
  for (let lat = -75; lat <= 75; lat += 15) {
    const y = toY(lat);
    ctx.beginPath();
    ctx.moveTo(0, y);
    ctx.lineTo(W, y);
    ctx.stroke();
  }

  ctx.strokeStyle = 'rgba(2, 132, 199, 0.52)';
  ctx.lineWidth = 2.2;
  ctx.beginPath();
  ctx.moveTo(0, toY(0));
  ctx.lineTo(W, toY(0));
  ctx.stroke();

  members.forEach((m) => {
    const cx = toX(m.geo_lng);
    const cy = toY(m.geo_lat);

    const ringColor = m.is_anchor
      ? 'rgba(2, 132, 199, 0.95)'
      : m.online
      ? 'rgba(5, 150, 105, 0.9)'
      : 'rgba(100, 116, 139, 0.75)';

    ctx.strokeStyle = ringColor;
    ctx.lineWidth = 2.0;

    ctx.beginPath();
    ctx.arc(cx, cy, 14, 0, Math.PI * 2);
    ctx.stroke();

    ctx.beginPath();
    ctx.arc(cx, cy, 6, 0, Math.PI * 2);
    ctx.stroke();
  });

  const texture = new THREE.CanvasTexture(canvas);
  texture.colorSpace = THREE.SRGBColorSpace;
  texture.anisotropy = maxAnisotropy;
  texture.needsUpdate = true;
  return texture;
}

function computeGlobeCameraPose(
  lat: number,
  lng: number,
  distanceFromCenter: number,
  lookAtRadius: number,
  outCamPos: THREE.Vector3,
  outLookAt: THREE.Vector3
) {
  const camLat = THREE.MathUtils.clamp(lat - (distanceFromCenter < 11 ? 5.5 : 0), -78, 78);
  const camLng = lng + (distanceFromCenter < 11 ? 4.5 : 0);

  latLngToVector3(camLat, camLng, distanceFromCenter, outCamPos);
  latLngToVector3(lat, lng, lookAtRadius, outLookAt);
}


export const IsometricMeshCockpit: React.FC<IsometricMeshCockpitProps> = ({
  zones,
  members,
  isCached,
  selectedZoneId,
  selectedDeviceId,
  onSelectZone,
  onSelectDevice,
  activeCommEvent,
  ambientIntensity = 75,
  onChangeAmbientIntensity,
  resolvedDark = true,
}) => {
  const canvasMountRef = useRef<HTMLDivElement | null>(null);
  const [viewMode, setViewMode] = useState<'globe' | 'isometric_lab'>('globe');
  // Tracks whether the globe camera is currently zoomed out beyond CLUSTER_ZOOM_DISTANCE_THRESHOLD
  const [isZoomedOutClusterView, setIsZoomedOutClusterView] = useState<boolean>(true);
  const [nodeScaleMultiplier, setNodeScaleMultiplier] = useState<number>(1.0);
  const [explodeFactor, setExplodeFactor] = useState<number>(0.2);
  const [showLabels, setShowLabels] = useState<boolean>(true);
  const [showCommPulses, setShowCommPulses] = useState<boolean>(true);
  const [autoRotateGlobe, setAutoRotateGlobe] = useState<boolean>(false);
  const [dockCollapsed, setDockCollapsed] = useState<boolean>(false);
  const [cameraPreset, setCameraPreset] = useState<
    'global' | 'apac' | 'euro' | 'zone' | 'device' | 'free'
  >('global');
  const [hoveredEntityId, setHoveredEntityId] = useState<string | null>(null);
  const [projectedLabels, setProjectedLabels] = useState<Record<string, ProjectedLabel>>({});

  // There is exactly one source of members: the roster the anchor pushed.
  // The prototype injected two synthetic Hong Kong nodes here behind a
  // default-ON toggle, which meant every operator opening the panel saw two
  // devices that do not exist — the dashboard painted a network instead of
  // reporting one (doubao-cloud-vm-01, round 58).
  const effectiveMembers = members;

  const cameraAnimatingRef = useRef<boolean>(false);
  const prevSelectionRef = useRef<{
    zoneId: string | null;
    deviceId: string | null;
    viewMode: 'globe' | 'isometric_lab';
  }>({
    zoneId: null,
    deviceId: null,
    viewMode: 'globe',
  });

  useEffect(() => {
    computeGlobeCameraPose(28, 82, 14.2, 0, _targetCameraPos, _targetLookAt);
  }, []);

  const stateRef = useRef({
    zones,
    members: effectiveMembers,
    isCached,
    selectedZoneId,
    selectedDeviceId,
    activeCommEvent,
    viewMode,
    nodeScaleMultiplier,
    explodeFactor,
    showCommPulses,
    autoRotateGlobe,
    cameraPreset,
    onSelectZone,
    onSelectDevice,
  });

  useEffect(() => {
    stateRef.current = {
      zones,
      members: effectiveMembers,
      isCached,
      selectedZoneId,
      selectedDeviceId,
      activeCommEvent,
      viewMode,
        nodeScaleMultiplier,
      explodeFactor,
      showCommPulses,
      autoRotateGlobe,
      cameraPreset,
      onSelectZone,
      onSelectDevice,
    };
  }, [
    zones,
    effectiveMembers,
    isCached,
    selectedZoneId,
    selectedDeviceId,
    activeCommEvent,
    viewMode,
    nodeScaleMultiplier,
    explodeFactor,
    showCommPulses,
    autoRotateGlobe,
    cameraPreset,
    onSelectZone,
    onSelectDevice,
  ]);

  // Trigger smooth camera fly-to when selectedZoneId, selectedDeviceId, or viewMode changes
  useEffect(() => {
    const prev = prevSelectionRef.current;
    if (
      prev.zoneId === selectedZoneId &&
      prev.deviceId === selectedDeviceId &&
      prev.viewMode === viewMode
    ) {
      return;
    }
    prevSelectionRef.current = {
      zoneId: selectedZoneId,
      deviceId: selectedDeviceId,
      viewMode,
    };

    if (viewMode === 'globe') {
      if (selectedDeviceId) {
        const dev = effectiveMembers.find((m) => m.device_id === selectedDeviceId);
        if (dev) {
          // Use the node's spread coordinate so camera centers on the exact satellite slot in a multi-node city
          const spread = computeSameCitySpreadLatLng(dev, effectiveMembers, 3.1);
          computeGlobeCameraPose(
            spread.lat,
            spread.lng,
            7.4,
            GLOBE_RADIUS * 0.88,
            _targetCameraPos,
            _targetLookAt
          );
          cameraAnimatingRef.current = true;
          setCameraPreset('device');
          return;
        }
      }

      if (selectedZoneId) {
        const zone = zones.find((z) => z.zone_id === selectedZoneId);
        if (zone) {
          computeGlobeCameraPose(
            zone.geo_lat,
            zone.geo_lng,
            9.4,
            GLOBE_RADIUS * 0.72,
            _targetCameraPos,
            _targetLookAt
          );
          cameraAnimatingRef.current = true;
          setCameraPreset('zone');
          return;
        }
      }

      computeGlobeCameraPose(28, 82, 14.2, 0, _targetCameraPos, _targetLookAt);
      cameraAnimatingRef.current = true;
      setCameraPreset('global');
    } else {
      if (selectedDeviceId) {
        const dev = effectiveMembers.find((m) => m.device_id === selectedDeviceId);
        const zone = zones.find((z) => z.zone_id === dev?.zone_id);
        if (dev && zone) {
          const zPeers = effectiveMembers.filter((m) => m.zone_id === zone.zone_id);
          const idxInZ = Math.max(0, zPeers.findIndex((m) => m.device_id === dev.device_id));
          const offset = getLabDeviceLocalOffset(dev.device_id, idxInZ, zPeers.length);
          const wx = zone.position_3d[0] + offset[0];
          const wy = zone.position_3d[1] + offset[1];
          const wz = zone.position_3d[2] + offset[2];
          _targetLookAt.set(wx, wy + 0.3, wz);
          _targetCameraPos.set(wx + 4.8, wy + 4.6, wz + 4.8);
          cameraAnimatingRef.current = true;
          setCameraPreset('device');
          return;
        }
      }

      if (selectedZoneId) {
        const zone = zones.find((z) => z.zone_id === selectedZoneId);
        if (zone) {
          const [zx, zy, zz] = zone.position_3d;
          _targetLookAt.set(zx, zy + 0.4, zz);
          _targetCameraPos.set(zx + 6.8, zy + 6.5, zz + 6.8);
          cameraAnimatingRef.current = true;
          setCameraPreset('zone');
          return;
        }
      }

      _targetLookAt.set(0, 0.2, 0.2);
      _targetCameraPos.set(11.5, 10.2, 11.5);
      cameraAnimatingRef.current = true;
      setCameraPreset('global');
    }
  }, [selectedZoneId, selectedDeviceId, viewMode, zones, effectiveMembers]);

  const handleSetCameraPreset = (preset: 'global' | 'apac' | 'euro') => {
    setCameraPreset(preset);
    if (viewMode === 'globe') {
      if (preset === 'global') {
        onSelectDevice(null);
        onSelectZone(null);
        computeGlobeCameraPose(28, 82, 14.2, 0, _targetCameraPos, _targetLookAt);
        cameraAnimatingRef.current = true;
      } else if (preset === 'apac') {
        computeGlobeCameraPose(22.3, 116, 9.6, GLOBE_RADIUS * 0.65, _targetCameraPos, _targetLookAt);
        cameraAnimatingRef.current = true;
      } else if (preset === 'euro') {
        computeGlobeCameraPose(49.5, 11.5, 9.8, GLOBE_RADIUS * 0.65, _targetCameraPos, _targetLookAt);
        cameraAnimatingRef.current = true;
      }
    } else {
      if (preset === 'global') {
        onSelectDevice(null);
        onSelectZone(null);
        _targetLookAt.set(0, 0.2, 0.2);
        _targetCameraPos.set(11.5, 10.2, 11.5);
        cameraAnimatingRef.current = true;
      } else if (preset === 'apac') {
        _targetLookAt.set(4.0, 0.4, 2.0);
        _targetCameraPos.set(9.8, 6.5, 7.8);
        cameraAnimatingRef.current = true;
      } else if (preset === 'euro') {
        _targetLookAt.set(-4.0, 0.4, 2.0);
        _targetCameraPos.set(1.8, 6.5, 7.8);
        cameraAnimatingRef.current = true;
      }
    }
  };

  useEffect(() => {
    const container = canvasMountRef.current;
    if (!container) return;

    const isSoftwareGl = (() => {
      const probeCanvas = document.createElement('canvas');
      const gl = probeCanvas.getContext('webgl2');
      if (!gl) return true;
      const ext = gl.getExtension('WEBGL_debug_renderer_info');
      const param = ext ? ext.UNMASKED_RENDERER_WEBGL : gl.RENDERER;
      const gpu = String(gl.getParameter(param));
      gl.getExtension('WEBGL_lose_context')?.loseContext();
      return /SwiftShader|llvmpipe|softpipe|Software|Basic Render/i.test(gpu);
    })();

    const width = container.clientWidth || 960;
    const height = container.clientHeight || 620;

    const scene = new THREE.Scene();
    scene.background = null;

    const camera = new THREE.PerspectiveCamera(36, width / height, 0.1, 140);
    computeGlobeCameraPose(28, 82, 14.2, 0, _targetCameraPos, _targetLookAt);
    camera.position.copy(_targetCameraPos);
    camera.lookAt(_currentLookAt);

    const renderer = new THREE.WebGLRenderer({
      antialias: !isSoftwareGl,
      alpha: true,
      powerPreference: 'high-performance',
    });
    renderer.setClearColor(0x000000, 0);
    renderer.outputColorSpace = THREE.SRGBColorSpace;
    renderer.toneMapping = THREE.ACESFilmicToneMapping;
    renderer.toneMappingExposure = 1.08;
    renderer.setPixelRatio(isSoftwareGl ? 1 : Math.min(window.devicePixelRatio, 2));
    renderer.setSize(width, height);
    renderer.shadowMap.enabled = !isSoftwareGl;
    renderer.shadowMap.type = THREE.PCFSoftShadowMap;

    container.innerHTML = '';
    container.appendChild(renderer.domElement);

    const pmremGenerator = new THREE.PMREMGenerator(renderer);
    scene.environment = pmremGenerator.fromScene(new RoomEnvironment(), 0.04).texture;
    pmremGenerator.dispose();

    const hemiLight = new THREE.HemisphereLight(0xffffff, 0xdbeafe, 0.95);
    hemiLight.position.set(0, 22, 0);
    scene.add(hemiLight);

    const keyLight = new THREE.DirectionalLight(0xfffbeb, 1.35);
    keyLight.position.set(14, 18, 16);
    keyLight.castShadow = !isSoftwareGl;
    keyLight.shadow.mapSize.width = 2048;
    keyLight.shadow.mapSize.height = 2048;
    keyLight.shadow.camera.near = 2;
    keyLight.shadow.camera.far = 45;
    const d = 10;
    keyLight.shadow.camera.left = -d;
    keyLight.shadow.camera.right = d;
    keyLight.shadow.camera.top = d;
    keyLight.shadow.camera.bottom = -d;
    keyLight.shadow.bias = -0.0005;
    keyLight.shadow.normalBias = 0.02;
    scene.add(keyLight);

    const rimLight = new THREE.DirectionalLight(0x38bdf8, 0.65);
    rimLight.position.set(-16, -8, -14);
    scene.add(rimLight);

    const matMatteWhite = new THREE.MeshStandardMaterial({
      color: 0xffffff,
      roughness: 0.55,
      metalness: 0.08,
    });
    const matBrushedMetal = new THREE.MeshPhysicalMaterial({
      color: 0xdbe2ea,
      roughness: 0.25,
      metalness: 0.82,
      clearcoat: 0.3,
    });
    const matDarkMetal = new THREE.MeshStandardMaterial({
      color: 0x334155,
      roughness: 0.38,
      metalness: 0.72,
    });
    const matGlassEnclosure = new THREE.MeshPhysicalMaterial({
      color: 0xe0f2fe,
      transparent: true,
      opacity: isSoftwareGl ? 0.24 : 0.88,
      transmission: isSoftwareGl ? 0 : 0.8,
      roughness: 0.12,
      metalness: 0.05,
      ior: 1.48,
      thickness: 0.35,
      depthWrite: false,
    });

    // 5A. Build Multi-Layer 3D Scientific Holographic Globe Environment
    const globeEnvGroup = new THREE.Group();
    scene.add(globeEnvGroup);

    const maxAnisotropy = renderer.capabilities.getMaxAnisotropy();
    const earthTexture = createHolographicEarthTexture(maxAnisotropy, stateRef.current.members);

    const globeGeo = new THREE.SphereGeometry(GLOBE_RADIUS, 72, 54);
    const globeMat = new THREE.MeshPhysicalMaterial({
      map: earthTexture,
      roughness: 0.45,
      metalness: 0.08,
      clearcoat: 0.25,
      clearcoatRoughness: 0.2,
    });
    const globeMesh = new THREE.Mesh(globeGeo, globeMat);
    globeMesh.receiveShadow = true;
    globeEnvGroup.add(globeMesh);

    const wireGeo = new THREE.SphereGeometry(GLOBE_RADIUS + 0.025, 36, 24);
    const wireMat = new THREE.MeshBasicMaterial({
      color: 0x0284c7,
      wireframe: true,
      transparent: true,
      opacity: 0.09,
      depthWrite: false,
    });
    const wireMesh = new THREE.Mesh(wireGeo, wireMat);
    globeEnvGroup.add(wireMesh);

    const atmosGeo = new THREE.SphereGeometry(GLOBE_RADIUS + 0.16, 48, 36);
    const atmosMat = new THREE.MeshBasicMaterial({
      color: 0x38bdf8,
      side: THREE.BackSide,
      transparent: true,
      opacity: 0.08,
      depthWrite: false,
    });
    const atmosMesh = new THREE.Mesh(atmosGeo, atmosMat);
    globeEnvGroup.add(atmosMesh);

    const orbitalGimbalGroup = new THREE.Group();
    const eqRingGeo = new THREE.RingGeometry(GLOBE_RADIUS + 0.68, GLOBE_RADIUS + 0.73, 128);
    const eqRingMat = new THREE.MeshBasicMaterial({
      color: 0x0284c7,
      side: THREE.DoubleSide,
      transparent: true,
      opacity: 0.28,
      depthWrite: false,
    });
    const eqRing = new THREE.Mesh(eqRingGeo, eqRingMat);
    eqRing.rotation.x = Math.PI / 2;
    orbitalGimbalGroup.add(eqRing);

    const polarRingGeo = new THREE.TorusGeometry(GLOBE_RADIUS + 0.88, 0.012, 12, 128);
    const polarRingMat = new THREE.MeshBasicMaterial({
      color: 0x64748b,
      transparent: true,
      opacity: 0.22,
    });
    const polarRing = new THREE.Mesh(polarRingGeo, polarRingMat);
    polarRing.rotation.x = THREE.MathUtils.degToRad(62);
    polarRing.rotation.y = THREE.MathUtils.degToRad(24);
    orbitalGimbalGroup.add(polarRing);
    globeEnvGroup.add(orbitalGimbalGroup);

    // 5B. Build Isometric Laboratory Platter Environment
    const labEnvGroup = new THREE.Group();
    labEnvGroup.visible = false;
    scene.add(labEnvGroup);

    const floorGeo = new THREE.PlaneGeometry(42, 42);
    const floorMat = new THREE.MeshStandardMaterial({
      color: 0xf8fafc,
      roughness: 0.88,
      metalness: 0.04,
    });
    const floorMesh = new THREE.Mesh(floorGeo, floorMat);
    floorMesh.rotation.x = -Math.PI / 2;
    floorMesh.position.y = -0.02;
    floorMesh.receiveShadow = true;
    labEnvGroup.add(floorMesh);

    const gridHelper = new THREE.GridHelper(26, 26, 0xcbd5e1, 0xe2e8f0);
    gridHelper.position.y = 0.005;
    labEnvGroup.add(gridHelper);

    // 6. Regional Zone Beacons (Globe Mode) & Zone Platforms (Lab Mode)
    const zoneGroups: Record<
      string,
      {
        globeGroup: THREE.Group;
        globeCanopy: THREE.Mesh;
        globeRing: THREE.Mesh;
        globeClusterHalo: THREE.Mesh;
        labGroup: THREE.Group;
        labCanopy: THREE.Mesh;
        labBorderFrame: THREE.LineSegments;
        surfaceNormal: THREE.Vector3;
        globeLabelPos: THREE.Vector3;
        labLabelPos: THREE.Vector3;
      }
    > = {};

    zones.forEach((zone) => {
      const isAnchorZone = zone.zone_id === 'zone_anchor_hub';

      const gGroup = new THREE.Group();
      const surfacePos = latLngToVector3(zone.geo_lat, zone.geo_lng, GLOBE_RADIUS + 0.012);
      const normal = surfacePos.clone().normalize();
      gGroup.position.copy(surfacePos);
      gGroup.quaternion.setFromUnitVectors(_upVec, normal);

      // Compact regional calibration ring on the globe surface
      const zoneRadius = isAnchorZone ? 0.44 : 0.64;
      const ringGeo = new THREE.RingGeometry(zoneRadius * 0.84, zoneRadius, 64);
      const ringMat = new THREE.MeshBasicMaterial({
        color: isAnchorZone ? 0x0284c7 : 0x64748b,
        side: THREE.DoubleSide,
        transparent: true,
        opacity: 0.45,
        depthWrite: false,
      });
      const borderRing = new THREE.Mesh(ringGeo, ringMat);
      borderRing.rotation.x = -Math.PI / 2;
      borderRing.position.y = 0.01;
      gGroup.add(borderRing);

      // Outer concentric pulsing halo for Smart Regional Cluster Ring mode
      const clusterHaloGeo = new THREE.RingGeometry(zoneRadius * 1.15, zoneRadius * 1.32, 64);
      const clusterHaloMat = new THREE.MeshBasicMaterial({
        color: isAnchorZone ? 0x38bdf8 : 0x10b981,
        side: THREE.DoubleSide,
        transparent: true,
        opacity: 0.0,
        depthWrite: false,
        blending: THREE.AdditiveBlending,
      });
      const clusterHaloRing = new THREE.Mesh(clusterHaloGeo, clusterHaloMat);
      clusterHaloRing.rotation.x = -Math.PI / 2;
      clusterHaloRing.position.y = 0.018;
      gGroup.add(clusterHaloRing);

      const domeGeo = new THREE.CylinderGeometry(
        zoneRadius * 0.72,
        zoneRadius * 0.92,
        0.18,
        36,
        1,
        true
      );
      const domeMesh = new THREE.Mesh(domeGeo, matGlassEnclosure.clone());
      domeMesh.position.y = 0.14;
      domeMesh.renderOrder = 3;
      domeMesh.visible = !isAnchorZone;
      gGroup.add(domeMesh);

      gGroup.userData = { entityType: 'zone', zoneId: zone.zone_id };
      globeEnvGroup.add(gGroup);

      // Isometric Lab Zone Platform
      const lGroup = new THREE.Group();
      lGroup.position.set(zone.position_3d[0], zone.position_3d[1], zone.position_3d[2]);
      const slabW = isAnchorZone ? 3.8 : 4.4;
      const slabD = isAnchorZone ? 3.0 : 3.4;

      const baseGeo = new THREE.BoxGeometry(slabW, 0.22, slabD);
      const baseMesh = new THREE.Mesh(baseGeo, matMatteWhite);
      baseMesh.position.y = 0.11;
      baseMesh.castShadow = true;
      baseMesh.receiveShadow = true;
      lGroup.add(baseMesh);

      const trimGeo = new THREE.BoxGeometry(slabW + 0.12, 0.08, slabD + 0.12);
      const trimMesh = new THREE.Mesh(trimGeo, matBrushedMetal);
      trimMesh.position.y = 0.04;
      lGroup.add(trimMesh);

      const canopyGeo = new THREE.BoxGeometry(slabW - 0.16, 1.45, slabD - 0.16);
      const canopyMesh = new THREE.Mesh(canopyGeo, matGlassEnclosure.clone());
      canopyMesh.position.y = 0.96;
      canopyMesh.renderOrder = 3;
      canopyMesh.visible = !isAnchorZone;
      lGroup.add(canopyMesh);

      const edgesGeo = new THREE.EdgesGeometry(canopyGeo);
      const edgesMat = new THREE.LineBasicMaterial({
        color: isAnchorZone ? 0x0284c7 : 0x94a3b8,
        transparent: true,
        opacity: isAnchorZone ? 0.35 : 0.55,
      });
      const borderFrame = new THREE.LineSegments(edgesGeo, edgesMat);
      borderFrame.position.copy(canopyMesh.position);
      lGroup.add(borderFrame);

      lGroup.userData = { entityType: 'zone', zoneId: zone.zone_id };
      labEnvGroup.add(lGroup);

      zoneGroups[zone.zone_id] = {
        globeGroup: gGroup,
        globeCanopy: domeMesh,
        globeRing: borderRing,
        globeClusterHalo: clusterHaloRing,
        labGroup: lGroup,
        labCanopy: canopyMesh,
        labBorderFrame: borderFrame,
        surfaceNormal: normal,
        globeLabelPos: latLngToVector3(zone.geo_lat, zone.geo_lng, GLOBE_RADIUS + 0.55),
        labLabelPos: new THREE.Vector3(zone.position_3d[0], 1.95, zone.position_3d[2]),
      };
    });

    // 7. Build the container server nodes: exactly the members the anchor reported.
    const allPossibleMembers = members;

    const deviceNodes: Record<
      string,
      {
        rootGroup: THREE.Group;
        coreGroup: THREE.Group;
        primaryColorMeshes: THREE.Mesh[];
        glassShell: THREE.Mesh | null;
        statusRing: THREE.Mesh;
        antennaGroup: THREE.Group;
        selectionHalo: THREE.Mesh;
        targetLaserBeam: THREE.Mesh;
        surfaceNormal: THREE.Vector3;
        globePos: THREE.Vector3;
        globeQuat: THREE.Quaternion;
        globeLabelPos: THREE.Vector3;
        labPos: THREE.Vector3;
        labLabelPos: THREE.Vector3;
        currentSpreadDeg: number;
      }
    > = {};

    allPossibleMembers.forEach((member) => {
      const surfacePos = latLngToVector3(member.geo_lat, member.geo_lng, GLOBE_RADIUS);
      const normal = surfacePos.clone().normalize();
      const globeQuat = new THREE.Quaternion().setFromUnitVectors(_upVec, normal);

      const zone = stateRef.current.zones.find((z) => z.zone_id === member.zone_id);
      const labPos = new THREE.Vector3(
        zone ? zone.position_3d[0] : 0,
        0.22,
        zone ? zone.position_3d[2] : 0
      );

      const dGroup = new THREE.Group();
      dGroup.position.copy(surfacePos);
      dGroup.quaternion.copy(globeQuat);
      dGroup.scale.setScalar(GLOBE_DEVICE_BASE_SCALE);

      dGroup.userData = {
        entityType: 'device',
        deviceId: member.device_id,
        zoneId: member.zone_id,
      };

      // Hit-proxy cylinder sized proportionally to the micro-node so adjacent same-city nodes don't steal clicks
      const hitProxyGeo = new THREE.CylinderGeometry(0.75, 0.75, 1.55, 16);
      const hitProxyMat = new THREE.MeshBasicMaterial({ visible: false });
      const hitProxy = new THREE.Mesh(hitProxyGeo, hitProxyMat);
      hitProxy.position.y = 0.75;
      dGroup.add(hitProxy);

      // 1. Metallic Geolocation Flange Pedestal
      const flangeGeo = new THREE.CylinderGeometry(0.58, 0.66, 0.14, 32);
      const flangeMesh = new THREE.Mesh(flangeGeo, matDarkMetal);
      flangeMesh.position.y = 0.07;
      flangeMesh.castShadow = true;
      flangeMesh.receiveShadow = true;
      dGroup.add(flangeMesh);

      // 2. Emissive Semantic Status Ring on Base
      const ringGeo = new THREE.TorusGeometry(0.56, 0.048, 16, 48);
      const ringMat = new THREE.MeshStandardMaterial({
        color: 0x059669,
        emissive: 0x059669,
        emissiveIntensity: 0.75,
        roughness: 0.3,
      });
      const statusRing = new THREE.Mesh(ringGeo, ringMat);
      statusRing.rotation.x = Math.PI / 2;
      statusRing.position.y = 0.15;
      dGroup.add(statusRing);

      // 3. Archetype-Specific Core Structure & Optional NAT Glass Enclosure
      const coreGroup = new THREE.Group();
      const primaryColorMeshes: THREE.Mesh[] = [];
      let glassShell: THREE.Mesh | null = null;

      const bodyMat = new THREE.MeshStandardMaterial({
        color: 0xffffff,
        roughness: 0.42,
        metalness: 0.16,
      });

      if (member.container_archetype === 'public_anchor_tower') {
        const towerCore = new THREE.Mesh(
          new THREE.CylinderGeometry(0.36, 0.48, 1.05, 8),
          bodyMat
        );
        towerCore.position.y = 0.6;
        towerCore.castShadow = true;
        coreGroup.add(towerCore);
        primaryColorMeshes.push(towerCore);

        [0.38, 0.64, 0.9].forEach((ry, idx) => {
          const dishRing = new THREE.Mesh(
            new THREE.TorusGeometry(0.5 - idx * 0.05, 0.035, 12, 32),
            matBrushedMetal
          );
          dishRing.rotation.x = Math.PI / 2;
          dishRing.position.y = ry;
          coreGroup.add(dishRing);
        });
      } else if (member.container_archetype === 'public_direct_compute') {
        const rackPlinth = new THREE.Mesh(
          new THREE.BoxGeometry(0.84, 0.12, 0.84),
          matBrushedMetal
        );
        rackPlinth.position.y = 0.18;
        coreGroup.add(rackPlinth);

        [-0.27, -0.09, 0.09, 0.27].forEach((bx) => {
          const blade = new THREE.Mesh(
            new THREE.BoxGeometry(0.11, 0.78, 0.74),
            bodyMat.clone()
          );
          blade.position.set(bx, 0.6, 0);
          blade.castShadow = true;
          coreGroup.add(blade);
          primaryColorMeshes.push(blade);

          const pipeBar = new THREE.Mesh(
            new THREE.BoxGeometry(0.12, 0.05, 0.76),
            matDarkMetal
          );
          pipeBar.position.set(bx, 1.0, 0);
          coreGroup.add(pipeBar);
        });
      } else if (member.container_archetype === 'nat_traversal_workstation') {
        const baseDeck = new THREE.Mesh(
          new THREE.BoxGeometry(0.76, 0.36, 0.76),
          bodyMat
        );
        baseDeck.position.y = 0.32;
        baseDeck.castShadow = true;
        coreGroup.add(baseDeck);
        primaryColorMeshes.push(baseDeck);

        const upperBridge = new THREE.Mesh(
          new THREE.CylinderGeometry(0.28, 0.36, 0.4, 6),
          matBrushedMetal
        );
        upperBridge.position.y = 0.68;
        coreGroup.add(upperBridge);

        const collarGeo = new THREE.CylinderGeometry(0.54, 0.54, 0.56, 32, 1, true);
        glassShell = new THREE.Mesh(collarGeo, matGlassEnclosure.clone());
        glassShell.position.y = 0.44;
        glassShell.renderOrder = 2;
        dGroup.add(glassShell);
      } else if (member.container_archetype === 'nat_restricted_sandbox') {
        const isoCore = new THREE.Mesh(
          new THREE.OctahedronGeometry(0.35, 0),
          bodyMat
        );
        isoCore.position.y = 0.56;
        isoCore.castShadow = true;
        coreGroup.add(isoCore);
        primaryColorMeshes.push(isoCore);

        const clampRing = new THREE.Mesh(
          new THREE.TorusGeometry(0.36, 0.028, 12, 24),
          matBrushedMetal
        );
        clampRing.rotation.x = Math.PI / 2;
        clampRing.position.y = 0.56;
        coreGroup.add(clampRing);

        const boxGlassGeo = new THREE.BoxGeometry(0.86, 0.94, 0.86);
        glassShell = new THREE.Mesh(boxGlassGeo, matGlassEnclosure.clone());
        glassShell.position.y = 0.58;
        glassShell.renderOrder = 2;
        dGroup.add(glassShell);
      } else {
        [-0.19, 0.19].forEach((sx) => {
          const silo = new THREE.Mesh(
            new THREE.CylinderGeometry(0.17, 0.17, 0.7, 24),
            bodyMat.clone()
          );
          silo.position.set(sx, 0.5, 0);
          silo.castShadow = true;
          coreGroup.add(silo);
          primaryColorMeshes.push(silo);

          const cap = new THREE.Mesh(
            new THREE.CylinderGeometry(0.18, 0.18, 0.06, 24),
            matBrushedMetal
          );
          cap.position.set(sx, 0.86, 0);
          coreGroup.add(cap);
        });

        const domeGeo = new THREE.CylinderGeometry(0.52, 0.52, 0.88, 32);
        glassShell = new THREE.Mesh(domeGeo, matGlassEnclosure.clone());
        glassShell.position.y = 0.58;
        glassShell.renderOrder = 2;
        dGroup.add(glassShell);
      }

      dGroup.add(coreGroup);

      // 4. Endpoint Announcement Antenna Array
      const antennaGroup = new THREE.Group();
      const stemGeo = new THREE.CylinderGeometry(0.032, 0.032, 0.38, 12);
      const stemMesh = new THREE.Mesh(stemGeo, matBrushedMetal);
      stemMesh.position.y = 1.15;
      antennaGroup.add(stemMesh);

      const beaconGeo = new THREE.TorusGeometry(0.2, 0.028, 12, 32);
      const beaconMat = new THREE.MeshStandardMaterial({
        color: 0x0284c7,
        emissive: 0x06b6d4,
        emissiveIntensity: 0.85,
      });
      const beaconMesh = new THREE.Mesh(beaconGeo, beaconMat);
      beaconMesh.rotation.x = Math.PI / 2;
      beaconMesh.position.y = 1.32;
      antennaGroup.add(beaconMesh);
      dGroup.add(antennaGroup);

      // 5. Geolocation Target Lock Ring & Vertical Laser Calibration Beam
      const haloGeo = new THREE.RingGeometry(0.76, 0.98, 48);
      const haloMat = new THREE.MeshBasicMaterial({
        color: 0x0284c7,
        side: THREE.DoubleSide,
        transparent: true,
        opacity: 0.0,
        depthWrite: false,
      });
      const selectionHalo = new THREE.Mesh(haloGeo, haloMat);
      selectionHalo.rotation.x = -Math.PI / 2;
      selectionHalo.position.y = 0.03;
      dGroup.add(selectionHalo);

      const beamGeo = new THREE.CylinderGeometry(0.02, 0.08, 2.4, 16, 1, true);
      const beamMat = new THREE.MeshBasicMaterial({
        color: 0x0ea5e9,
        transparent: true,
        opacity: 0.0,
        side: THREE.DoubleSide,
        depthWrite: false,
      });
      const targetLaserBeam = new THREE.Mesh(beamGeo, beamMat);
      targetLaserBeam.position.y = 1.2;
      dGroup.add(targetLaserBeam);

      scene.add(dGroup);

      deviceNodes[member.device_id] = {
        rootGroup: dGroup,
        coreGroup,
        primaryColorMeshes,
        glassShell,
        statusRing,
        antennaGroup,
        selectionHalo,
        targetLaserBeam,
        surfaceNormal: normal,
        globePos: surfacePos,
        globeQuat,
        globeLabelPos: latLngToVector3(member.geo_lat, member.geo_lng, GLOBE_RADIUS + 0.42),
        labPos,
        labLabelPos: new THREE.Vector3(labPos.x, labPos.y + 1.25, labPos.z),
        currentSpreadDeg: 0,
      };
    });

    // 8. Global Geodesic Great-Circle Arcs (Globe Mode) & Lab Parabolas (Lab Mode)
    // Includes semi-transparent base conduit, UV-scrolling directional stream sheath, and 4-segment comet packet train
    const baseStreamTexture = createDirectionalArcFlowTexture();

    const linkPairs: Array<{
      id: string;
      fromId: string;
      toId: string;
      flowDirection: 1 | -1; // 1: from -> to, -1: to -> from
      baseIntensity: number; // 0.6 (light control) to 1.65 (heavy P2P data stream)
      globeCurve: THREE.CatmullRomCurve3;
      labCurve: THREE.QuadraticBezierCurve3;
      globeTubeMesh: THREE.Mesh;
      globeStreamMesh: THREE.Mesh;
      labTubeMesh: THREE.Mesh;
      labStreamMesh: THREE.Mesh;
      flowTexture: THREE.CanvasTexture;
      dirConeMesh: THREE.Mesh;
      trailMeshes: THREE.Mesh[];
    }> = [];

    // Draw a link only when the API explicitly reports the peer relation.
    // The current payload leaves peers_connected empty because cmesh does not
    // collect that fact yet. An empty relation must mean "no link rendered",
    // not "infer every online member is connected to the anchor".
    const pairDefinitions: Array<{
      fromId: string;
      toId: string;
      peakAltitude: number;
      labArch: number;
      flowDirection: 1 | -1;
      baseIntensity: number;
    }> = [];

    allPossibleMembers.forEach((member) => {
      if (member.is_anchor || !member.online) return;
      const peers = member.peers_connected ?? [];
      peers.filter((peerID) => peerID !== member.device_id).forEach((peerID) => {
        const peer = allPossibleMembers.find((candidate) => candidate.device_id === peerID);
        if (!peer || !peer.online) return;
        pairDefinitions.push({
          fromId: member.device_id,
          toId: peerID,
          peakAltitude: member.endpoints.length > 0 ? 1.35 : 0.72,
          labArch: 1.65,
          flowDirection: 1,
          baseIntensity: member.endpoints.length > 0 ? 1.15 : 0.85,
        });
      });
    });

    pairDefinitions.forEach(
      ({ fromId, toId, peakAltitude, labArch, flowDirection, baseIntensity }) => {
        const mA = allPossibleMembers.find((m) => m.device_id === fromId);
        const mB = allPossibleMembers.find((m) => m.device_id === toId);
        const nA = deviceNodes[fromId];
        const nB = deviceNodes[toId];
        if (!mA || !mB || !nA || !nB) return;

        // Use spread coordinates so co-located Hong Kong nodes have distinct arc endpoints
        const spreadA = computeSameCitySpreadLatLng(mA, allPossibleMembers, 2.2);
        const spreadB = computeSameCitySpreadLatLng(mB, allPossibleMembers, 2.2);

        const globeCurve = createGeodesicArcCurve(
          spreadA.lat,
          spreadA.lng,
          spreadB.lat,
          spreadB.lng,
          GLOBE_RADIUS + 0.14,
          peakAltitude
        );

        // 1. Base Semi-Transparent Geodesic Trajectory Conduit
        const globeTubeGeo = new THREE.TubeGeometry(globeCurve, 56, 0.014, 8, false);
        const baseTubeMat = new THREE.MeshStandardMaterial({
          color: 0x0ea5e9,
          emissive: 0x0284c7,
          emissiveIntensity: 0.35,
          roughness: 0.3,
          transparent: true,
          opacity: 0.36,
          depthWrite: false,
        });
        const globeTubeMesh = new THREE.Mesh(globeTubeGeo, baseTubeMat);
        globeEnvGroup.add(globeTubeMesh);

        // 2. Animated UV-Scrolling Directional Energy Stream Sheath
        const flowTexture = baseStreamTexture.clone();
        flowTexture.needsUpdate = true;
        const globeStreamGeo = new THREE.TubeGeometry(
          globeCurve,
          56,
          0.022 * Math.sqrt(baseIntensity),
          8,
          false
        );
        const streamTubeMat = new THREE.MeshBasicMaterial({
          color: 0x38bdf8,
          map: flowTexture,
          alphaMap: flowTexture,
          transparent: true,
          opacity: 0.88,
          depthWrite: false,
          blending: THREE.AdditiveBlending,
        });
        const globeStreamMesh = new THREE.Mesh(globeStreamGeo, streamTubeMat);
        globeEnvGroup.add(globeStreamMesh);

        // Lab Mode Curves & Tubes
        const startLab = nA.labPos.clone().add(new THREE.Vector3(0, 0.55, 0));
        const endLab = nB.labPos.clone().add(new THREE.Vector3(0, 0.55, 0));
        const midLab = new THREE.Vector3().addVectors(startLab, endLab).multiplyScalar(0.5);
        midLab.y += labArch;
        const labCurve = new THREE.QuadraticBezierCurve3(startLab, midLab, endLab);

        const labTubeGeo = new THREE.TubeGeometry(labCurve, 44, 0.02, 8, false);
        const labTubeMesh = new THREE.Mesh(labTubeGeo, baseTubeMat);
        labEnvGroup.add(labTubeMesh);

        const labStreamGeo = new THREE.TubeGeometry(
          labCurve,
          44,
          0.032 * Math.sqrt(baseIntensity),
          8,
          false
        );
        const labStreamMesh = new THREE.Mesh(labStreamGeo, streamTubeMat);
        labEnvGroup.add(labStreamMesh);

        // 3. Directional Arrow Cone Head (indicates exact direction of data flow along the arc)
        const coneGeo = new THREE.ConeGeometry(0.062, 0.17, 12);
        coneGeo.rotateX(Math.PI / 2); // Align cone tip with +Z for lookAt()
        const coneMat = new THREE.MeshBasicMaterial({
          color: 0x38bdf8,
          transparent: true,
          opacity: 0.95,
        });
        const dirConeMesh = new THREE.Mesh(coneGeo, coneMat);
        scene.add(dirConeMesh);

        // 4. 4-Segment Comet-Tail Packet Train (visualizes packet velocity & stream intensity)
        const trailRadii = [0.056, 0.044, 0.033, 0.022];
        const trailOpacities = [0.96, 0.72, 0.48, 0.25];
        const trailMeshes: THREE.Mesh[] = trailRadii.map((r, tIdx) => {
          const tGeo = new THREE.SphereGeometry(r, 12, 12);
          const tMat = new THREE.MeshBasicMaterial({
            color: 0x38bdf8,
            transparent: true,
            opacity: trailOpacities[tIdx],
          });
          const tMesh = new THREE.Mesh(tGeo, tMat);
          scene.add(tMesh);
          return tMesh;
        });

        linkPairs.push({
          id: `${fromId}__${toId}`,
          fromId,
          toId,
          flowDirection,
          baseIntensity,
          globeCurve,
          labCurve,
          globeTubeMesh,
          globeStreamMesh,
          labTubeMesh,
          labStreamMesh,
          flowTexture,
          dirConeMesh,
          trailMeshes,
        });
      }
    );

    // 9. OrbitControls & 3D Raycasting
    const controls = new OrbitControls(camera, renderer.domElement);
    controls.enableDamping = !isSoftwareGl;
    controls.dampingFactor = 0.07;
    controls.minPolarAngle = 0.12;
    controls.maxPolarAngle = Math.PI - 0.12;
    controls.minDistance = 5.5;
    controls.maxDistance = 25;
    controls.target.copy(_currentLookAt);

    controls.addEventListener('start', () => {
      cameraAnimatingRef.current = false;
      setCameraPreset('free');
    });

    const pickables: THREE.Object3D[] = [
      ...Object.values(deviceNodes).map((d) => d.rootGroup),
      ...Object.values(zoneGroups).map((z) => z.globeGroup),
      ...Object.values(zoneGroups).map((z) => z.labGroup),
    ];

    const findEntityFromHit = (
      obj: THREE.Object3D | null
    ): { entityType: 'device' | 'zone'; deviceId?: string; zoneId: string } | null => {
      let curr: THREE.Object3D | null = obj;
      while (curr) {
        if (!curr.visible) return null;
        if (curr.userData?.entityType === 'device') {
          return {
            entityType: 'device',
            deviceId: curr.userData.deviceId,
            zoneId: curr.userData.zoneId,
          };
        }
        if (curr.userData?.entityType === 'zone') {
          return {
            entityType: 'zone',
            zoneId: curr.userData.zoneId,
          };
        }
        curr = curr.parent;
      }
      return null;
    };

    let pointerDownX = 0;
    let pointerDownY = 0;
    let lastRaycastTime = 0;

    const handlePointerDown = (e: PointerEvent) => {
      pointerDownX = e.clientX;
      pointerDownY = e.clientY;
    };

    const handlePointerMove = (e: PointerEvent) => {
      const now = performance.now();
      if (now - lastRaycastTime < 40) return;
      lastRaycastTime = now;

      const rect = renderer.domElement.getBoundingClientRect();
      _pointerNdc.x = ((e.clientX - rect.left) / rect.width) * 2 - 1;
      _pointerNdc.y = -((e.clientY - rect.top) / rect.height) * 2 + 1;

      _raycaster.setFromCamera(_pointerNdc, camera);
      const hits = _raycaster.intersectObjects(pickables, true);
      if (hits.length > 0) {
        for (const hit of hits) {
          const info = findEntityFromHit(hit.object);
          if (info?.entityType === 'device' && info.deviceId) {
            setHoveredEntityId(info.deviceId);
            return;
          }
        }
        const firstInfo = findEntityFromHit(hits[0].object);
        if (firstInfo?.entityType === 'zone') {
          setHoveredEntityId(firstInfo.zoneId);
          return;
        }
      }
      setHoveredEntityId(null);
    };

    const handlePointerUp = (e: PointerEvent) => {
      const moveDist = Math.hypot(e.clientX - pointerDownX, e.clientY - pointerDownY);
      if (moveDist > 5) return;

      const rect = renderer.domElement.getBoundingClientRect();
      _pointerNdc.x = ((e.clientX - rect.left) / rect.width) * 2 - 1;
      _pointerNdc.y = -((e.clientY - rect.top) / rect.height) * 2 + 1;

      _raycaster.setFromCamera(_pointerNdc, camera);
      const hits = _raycaster.intersectObjects(pickables, true);

      if (hits.length > 0) {
        const camDist = camera.position.distanceTo(controls.target);
        const isZoomedOut =
          stateRef.current.viewMode === 'globe' && camDist > CLUSTER_ZOOM_DISTANCE_THRESHOLD;

        for (const hit of hits) {
          const info = findEntityFromHit(hit.object);
          if (info?.entityType === 'device' && info.deviceId) {
            // If this device belongs to a multi-node city cluster that is currently collapsed at low zoom,
            // clicking it first expands/scatters the regional cluster!
            const clickedMember = stateRef.current.members.find(
              (m) => m.device_id === info.deviceId
            );
            if (clickedMember && isZoomedOut) {
              const sameCityPeers = stateRef.current.members.filter(
                (other) =>
                  Math.hypot(
                    other.geo_lat - clickedMember.geo_lat,
                    other.geo_lng - clickedMember.geo_lng
                  ) < 2.5
              );
              const isClusterAlreadyFocused =
                stateRef.current.selectedZoneId === clickedMember.zone_id ||
                sameCityPeers.some((p) => p.device_id === stateRef.current.selectedDeviceId);

              if (sameCityPeers.length > 1 && !isClusterAlreadyFocused) {
                stateRef.current.onSelectZone(clickedMember.zone_id);
                stateRef.current.onSelectDevice(null);
                return;
              }
            }

            stateRef.current.onSelectZone(info.zoneId);
            stateRef.current.onSelectDevice(info.deviceId);
            return;
          }
        }
        const zoneInfo = findEntityFromHit(hits[0].object);
        if (zoneInfo?.entityType === 'zone') {
          if (stateRef.current.selectedDeviceId) {
            stateRef.current.onSelectDevice(null);
          } else {
            stateRef.current.onSelectZone(zoneInfo.zoneId);
            stateRef.current.onSelectDevice(null);
          }
          return;
        }
      }

      if (stateRef.current.selectedDeviceId) {
        stateRef.current.onSelectDevice(null);
      } else if (stateRef.current.selectedZoneId) {
        stateRef.current.onSelectZone(null);
      }
    };

    const domElem = renderer.domElement;
    domElem.addEventListener('pointerdown', handlePointerDown);
    domElem.addEventListener('pointermove', handlePointerMove);
    domElem.addEventListener('pointerup', handlePointerUp);

    if (!isSoftwareGl) {
      renderer.compile(scene, camera);
    }

    // 10. Animation & State Synchronization Loop
    let frameId = 0;
    let isUnmounted = false;
    const clock = new THREE.Clock();

    function afterDraw(cb: () => void) {
      const gl = renderer.getContext() as WebGL2RenderingContext;
      const sync = gl && gl.fenceSync ? gl.fenceSync(gl.SYNC_GPU_COMMANDS_COMPLETE, 0) : null;
      if (gl && gl.flush) gl.flush();
      const poll = () => {
        if (isUnmounted) return;
        const state = sync && gl.getSyncParameter(sync, gl.SYNC_STATUS);
        if (state === gl.UNSIGNALED) {
          frameId = window.setTimeout(poll, 5);
        } else {
          if (sync) gl.deleteSync(sync);
          frameId = window.setTimeout(cb, 80);
        }
      };
      frameId = window.setTimeout(poll, 5);
    }

    let labelUpdateCounter = 0;

    function tick() {
      if (isUnmounted) return;
      const delta = Math.min(clock.getDelta(), 0.1);
      const elapsed = clock.getElapsedTime();
      const current = stateRef.current;
      const isGlobeMode = current.viewMode === 'globe';

      globeEnvGroup.visible = isGlobeMode;
      labEnvGroup.visible = !isGlobeMode;

      controls.maxPolarAngle = isGlobeMode ? Math.PI - 0.12 : Math.PI / 2 - 0.06;

      polarRing.rotation.z = elapsed * 0.08;

      controls.autoRotate =
        !isSoftwareGl &&
        isGlobeMode &&
        current.autoRotateGlobe &&
        !current.selectedDeviceId &&
        !current.selectedZoneId &&
        !cameraAnimatingRef.current;
      controls.autoRotateSpeed = 0.65;

      if (cameraAnimatingRef.current) {
        const dampFactor = 1 - Math.exp(-5.5 * delta);
        camera.position.lerp(_targetCameraPos, dampFactor);
        _currentLookAt.lerp(_targetLookAt, dampFactor);
        controls.target.copy(_currentLookAt);

        if (
          camera.position.distanceToSquared(_targetCameraPos) < 0.003 &&
          controls.target.distanceToSquared(_targetLookAt) < 0.003
        ) {
          cameraAnimatingRef.current = false;
        }
      } else {
        _currentLookAt.copy(controls.target);
      }
      controls.update();

      // Measure camera distance to determine whether multi-device regions should aggregate into a Cluster Ring
      const cameraDist = camera.position.distanceTo(controls.target);
      const isCamZoomedOut = isGlobeMode && cameraDist > CLUSTER_ZOOM_DISTANCE_THRESHOLD;

      // Update Zones in active mode
      current.zones.forEach((zone) => {
        const zObj = zoneGroups[zone.zone_id];
        if (!zObj) return;
        const isZoneSelected = current.selectedZoneId === zone.zone_id;
        const zoneMembers = current.members.filter((m) => m.zone_id === zone.zone_id);
        const hasSelectedMemberInZone = zoneMembers.some(
          (m) => m.device_id === current.selectedDeviceId
        );
        const isMultiNodeZoneCollapsed =
          isCamZoomedOut &&
          zoneMembers.length > 1 &&
          !isZoneSelected &&
          !hasSelectedMemberInZone;

        if (isGlobeMode) {
          const targetCanopyY =
            0.14 + current.explodeFactor * 0.32 + (isZoneSelected ? 0.18 : 0);
          zObj.globeCanopy.position.y = THREE.MathUtils.lerp(
            zObj.globeCanopy.position.y,
            targetCanopyY,
            0.12
          );
          const ringMat = zObj.globeRing.material as THREE.MeshBasicMaterial;
          const haloMat = zObj.globeClusterHalo.material as THREE.MeshBasicMaterial;

          if (isMultiNodeZoneCollapsed) {
            // Highlight 3D Regional Cluster Ring on the globe surface with breathing pulse
            const pulse = 0.5 + 0.5 * Math.sin(elapsed * 3.4);
            ringMat.color.setHex(0x0ea5e9);
            ringMat.opacity = 0.88;
            zObj.globeRing.scale.setScalar(1.0 + pulse * 0.12);

            haloMat.color.setHex(current.isCached ? 0xf59e0b : 0x38bdf8);
            haloMat.opacity = 0.35 + pulse * 0.45;
            zObj.globeClusterHalo.scale.setScalar(0.95 + pulse * 0.28);
          } else {
            zObj.globeRing.scale.setScalar(1.0);
            haloMat.opacity = THREE.MathUtils.lerp(haloMat.opacity, 0.0, 0.18);

            if (isZoneSelected) {
              ringMat.color.setHex(0x0284c7);
              ringMat.opacity = 0.9;
            } else if (current.isCached) {
              ringMat.color.setHex(0xd97706);
              ringMat.opacity = 0.65;
            } else {
              ringMat.color.setHex(zone.zone_id === 'zone_anchor_hub' ? 0x0284c7 : 0x64748b);
              ringMat.opacity = 0.42;
            }
          }
        } else {
          const targetLabCanopyY =
            0.96 + current.explodeFactor * 0.85 + (isZoneSelected ? 0.55 : 0);
          zObj.labCanopy.position.y = THREE.MathUtils.lerp(
            zObj.labCanopy.position.y,
            targetLabCanopyY,
            0.12
          );
          zObj.labBorderFrame.position.y = zObj.labCanopy.position.y;
        }
      });

      // Hide nodes that are no longer in the roster (a member left the view).
      const activeIds = new Set(current.members.map((m) => m.device_id));
      Object.entries(deviceNodes).forEach(([id, node]) => {
        if (!activeIds.has(id)) {
          node.rootGroup.visible = false;
        }
      });

      // Update Active Server / Container Models with Smart Zoom-Level Cluster Aggregation & Scatter
      current.members.forEach((member) => {
        const dNode = deviceNodes[member.device_id];
        if (!dNode) return;

        const isSelected = current.selectedDeviceId === member.device_id;
        const isInSelectedZone = current.selectedZoneId === member.zone_id;
        const hasEndpoints = member.endpoints && member.endpoints.length > 0;

        // Check how many servers share this geographic area
        const sameCityPeers = current.members.filter(
          (other) =>
            Math.hypot(other.geo_lat - member.geo_lat, other.geo_lng - member.geo_lng) < 2.5
        );
        const isMultiNodeCluster = sameCityPeers.length > 1;
        const anyPeerSelected = sameCityPeers.some(
          (p) => p.device_id === current.selectedDeviceId
        );

        // Collapse into a single Regional Cluster Ring when zoomed out AND neither the zone nor any peer is selected
        const isClusterCollapsed =
          isGlobeMode &&
          isMultiNodeCluster &&
          isCamZoomedOut &&
          !isInSelectedZone &&
          !anyPeerSelected;

        // Target spread angle in degrees: 0° when collapsed into Cluster Ring, 3.4° when selected/focused, 2.6° when zoomed in
        const targetSpreadDeg = isClusterCollapsed
          ? 0.0
          : isInSelectedZone || anyPeerSelected
          ? 3.4
          : 2.6;

        dNode.currentSpreadDeg = THREE.MathUtils.lerp(
          dNode.currentSpreadDeg,
          targetSpreadDeg,
          0.14
        );

        const spreadCoord = computeSameCitySpreadLatLng(
          member,
          current.members,
          dNode.currentSpreadDeg
        );

        latLngToVector3(spreadCoord.lat, spreadCoord.lng, GLOBE_RADIUS, dNode.globePos);
        dNode.surfaceNormal.copy(dNode.globePos).normalize();
        dNode.globeQuat.setFromUnitVectors(_upVec, dNode.surfaceNormal);
        latLngToVector3(
          spreadCoord.lat,
          spreadCoord.lng,
          GLOBE_RADIUS + 0.42 * current.nodeScaleMultiplier,
          dNode.globeLabelPos
        );

        // Lab position for same-zone nodes
        const zone = current.zones.find((z) => z.zone_id === member.zone_id);
        const zonePeers = current.members.filter((m) => m.zone_id === member.zone_id);
        const idxInZone = Math.max(0, zonePeers.findIndex((m) => m.device_id === member.device_id));
        const labOffset = getLabDeviceLocalOffset(member.device_id, idxInZone, zonePeers.length);
        dNode.labPos.set(
          (zone ? zone.position_3d[0] : 0) + labOffset[0],
          labOffset[1],
          (zone ? zone.position_3d[2] : 0) + labOffset[2]
        );
        dNode.labLabelPos.set(dNode.labPos.x, dNode.labPos.y + 1.25, dNode.labPos.z);

        const targetPos = isGlobeMode ? dNode.globePos : dNode.labPos;
        const targetQuat = isGlobeMode ? dNode.globeQuat : _identityQuat;

        // When collapsed into a Regional Cluster Ring, keep only the primary node (index 0) as the central 3D core
        // and smoothly scale satellite sibling models down to 0 so they emerge from the center when scattering!
        const isCollapsedSatelliteNode = isClusterCollapsed && spreadCoord.clusterIndex > 0;
        const baseGlobeScale = isCollapsedSatelliteNode
          ? 0.001
          : isSelected
          ? GLOBE_DEVICE_FOCUSED_SCALE
          : isInSelectedZone
          ? 0.25
          : GLOBE_DEVICE_BASE_SCALE;

        const targetScale = isGlobeMode
          ? baseGlobeScale * current.nodeScaleMultiplier
          : LAB_DEVICE_SCALE * current.nodeScaleMultiplier;

        dNode.rootGroup.position.lerp(targetPos, 0.15);
        dNode.rootGroup.quaternion.slerp(targetQuat, 0.15);
        const currScale = THREE.MathUtils.lerp(dNode.rootGroup.scale.x, targetScale, 0.15);
        dNode.rootGroup.scale.setScalar(currScale);
        dNode.rootGroup.visible = currScale > 0.015;

        dNode.antennaGroup.visible = hasEndpoints;

        const targetCoreY = member.online ? 0.0 : -0.1;
        dNode.coreGroup.position.y = THREE.MathUtils.lerp(
          dNode.coreGroup.position.y,
          targetCoreY,
          0.12
        );

        if (dNode.glassShell) {
          const baseShellY =
            member.container_archetype === 'nat_traversal_workstation' ? 0.44 : 0.58;
          const targetShellY =
            baseShellY + current.explodeFactor * 0.45 + (isSelected ? 0.42 : 0);
          dNode.glassShell.position.y = THREE.MathUtils.lerp(
            dNode.glassShell.position.y,
            targetShellY,
            0.12
          );
        }

        const ringMat = dNode.statusRing.material as THREE.MeshStandardMaterial;
        const targetBodyColor = !member.online ? 0xe2e8f0 : 0xffffff;

        if (!member.online) {
          ringMat.color.setHex(0x64748b);
          ringMat.emissive.setHex(0x475569);
          ringMat.emissiveIntensity = 0.15;
        } else if (!hasEndpoints || member.transport === 'relay' || current.isCached) {
          ringMat.color.setHex(0xd97706);
          ringMat.emissive.setHex(0xf59e0b);
          ringMat.emissiveIntensity = 0.7;
        } else if (member.is_anchor) {
          ringMat.color.setHex(0x0284c7);
          ringMat.emissive.setHex(0x06b6d4);
          ringMat.emissiveIntensity = 0.9;
        } else {
          ringMat.color.setHex(0x059669);
          ringMat.emissive.setHex(0x10b981);
          ringMat.emissiveIntensity = 0.8;
        }

        dNode.primaryColorMeshes.forEach((mesh) => {
          (mesh.material as THREE.MeshStandardMaterial).color.setHex(targetBodyColor);
        });

        const haloMat = dNode.selectionHalo.material as THREE.MeshBasicMaterial;
        haloMat.opacity = isSelected ? 0.92 : 0.0;

        const beamMat = dNode.targetLaserBeam.material as THREE.MeshBasicMaterial;
        beamMat.opacity = isSelected && isGlobeMode ? 0.35 : 0.0;
      });

      // Update Geodesic Arc Paths, UV Stream Sheaths & Comet-Tail Packet Trains
      linkPairs.forEach((link, idx) => {
        const mA = current.members.find((m) => m.device_id === link.fromId);
        const mB = current.members.find((m) => m.device_id === link.toId);
        const baseMat = link.globeTubeMesh.material as THREE.MeshStandardMaterial;
        const streamMat = link.globeStreamMesh.material as THREE.MeshBasicMaterial;
        const coneMat = link.dirConeMesh.material as THREE.MeshBasicMaterial;

        // If either node is not in the currently active member list (e.g. HK demo nodes when off), hide everything
        if (!mA || !mB) {
          link.globeTubeMesh.visible = false;
          link.globeStreamMesh.visible = false;
          link.labTubeMesh.visible = false;
          link.labStreamMesh.visible = false;
          link.dirConeMesh.visible = false;
          link.trailMeshes.forEach((tm) => {
            tm.visible = false;
          });
          return;
        }

        link.globeTubeMesh.visible = isGlobeMode;
        link.labTubeMesh.visible = !isGlobeMode;

        const bothOnline = mA.online && mB.online;
        const connected =
          bothOnline &&
          (mA.peers_connected.includes(mB.device_id) ||
            mB.peers_connected.includes(mA.device_id));

        // Check if this specific link is currently experiencing a user-triggered probe surge
        const isSurging =
          Boolean(current.activeCommEvent) &&
          ((current.activeCommEvent?.fromId === link.fromId &&
            current.activeCommEvent?.toId === link.toId) ||
            (current.activeCommEvent?.fromId === link.toId &&
              current.activeCommEvent?.toId === link.fromId));

        if (!bothOnline) {
          // 1. OFFLINE STATE: Neutral Slate Gray semi-transparent trajectory, zero active stream
          baseMat.color.setHex(0x64748b);
          baseMat.emissive.setHex(0x475569);
          baseMat.emissiveIntensity = 0.08;
          baseMat.opacity = 0.22;

          link.globeStreamMesh.visible = false;
          link.labStreamMesh.visible = false;
          link.dirConeMesh.visible = false;
          link.trailMeshes.forEach((tm) => {
            tm.visible = false;
          });
        } else if (connected) {
          const isRelayOrNoEndpoint =
            mA.transport === 'relay' ||
            mB.transport === 'relay' ||
            mA.transport === 'pending' ||
            mB.transport === 'pending' ||
            mA.endpoints.length === 0 ||
            mB.endpoints.length === 0;
          const isDirectP2PPeerLink = !mA.is_anchor && !mB.is_anchor && !isRelayOrNoEndpoint;

          let pathColorHex = 0x0ea5e9; // Anchor Direct Channel (Sky Cyan)
          let glowColorHex = 0x38bdf8;
          let stateSpeedFactor = 1.0;

          if (current.isCached) {
            // DISCOVERY LOST / CACHED SNAPSHOT: Amber warning trajectory, stream paused
            pathColorHex = 0xd97706;
            glowColorHex = 0xf59e0b;
            stateSpeedFactor = 0.0;
          } else if (isRelayOrNoEndpoint) {
            // RELAY / OUTBOUND-ONLY NO-ENDPOINT: Warm Amber Gold stream
            pathColorHex = 0xd97706;
            glowColorHex = 0xfbbf24;
            stateSpeedFactor = 0.78;
          } else if (isDirectP2PPeerLink) {
            // DIRECT P2P DATA PLANE: Vibrant Emerald Green high-throughput stream
            pathColorHex = 0x059669;
            glowColorHex = 0x34d399;
            stateSpeedFactor = 1.35;
          } else {
            // ANCHOR DIRECT CONTROL/DATA CHANNEL: Sky Cyan stream
            pathColorHex = 0x0284c7;
            glowColorHex = 0x38bdf8;
            stateSpeedFactor = 1.05;
          }

          const effectiveIntensity =
            link.baseIntensity * stateSpeedFactor * (isSurging ? 2.1 : 1.0);

          baseMat.color.setHex(pathColorHex);
          baseMat.emissive.setHex(glowColorHex);
          baseMat.emissiveIntensity = isSurging ? 0.95 : 0.42;
          baseMat.opacity = current.isCached ? 0.45 : isSurging ? 0.72 : 0.38;

          const showAnimatedStream = current.showCommPulses && !current.isCached;
          link.globeStreamMesh.visible = isGlobeMode && showAnimatedStream;
          link.labStreamMesh.visible = !isGlobeMode && showAnimatedStream;
          link.dirConeMesh.visible = showAnimatedStream;

          if (showAnimatedStream) {
            streamMat.color.setHex(glowColorHex);
            streamMat.opacity = Math.min(1.0, 0.55 + effectiveIntensity * 0.25);

            // Scroll UV offset along the arc to animate continuous directional energy flow
            const scrollDelta = delta * 0.65 * effectiveIntensity * link.flowDirection;
            link.flowTexture.offset.x = (link.flowTexture.offset.x - scrollDelta) % 1;

            // Animate 4-segment comet packet train & oriented directional cone head
            const rawPhase = (elapsed * 0.28 * effectiveIntensity + idx * 0.19) % 1;
            const headT = link.flowDirection === 1 ? rawPhase : 1 - rawPhase;
            const activeCurve = isGlobeMode ? link.globeCurve : link.labCurve;

            // Position & orient directional arrow cone at the leading packet head
            activeCurve.getPoint(headT, link.dirConeMesh.position);
            const lookAheadT = THREE.MathUtils.clamp(
              headT + link.flowDirection * 0.025,
              0,
              1
            );
            activeCurve.getPoint(lookAheadT, _tangentLookVec);
            link.dirConeMesh.lookAt(_tangentLookVec);
            coneMat.color.setHex(glowColorHex);
            const coneScale = 0.85 + Math.min(0.65, effectiveIntensity * 0.28);
            link.dirConeMesh.scale.setScalar(coneScale);

            // Position trailing comet spheres behind the head
            link.trailMeshes.forEach((tMesh, tIdx) => {
              tMesh.visible = true;
              const lag = (tIdx + 1) * 0.026 * link.flowDirection;
              const segT = (headT - lag + 1) % 1;
              activeCurve.getPoint(segT, tMesh.position);
              (tMesh.material as THREE.MeshBasicMaterial).color.setHex(glowColorHex);
              tMesh.scale.setScalar(coneScale);
            });
          } else {
            link.trailMeshes.forEach((tm) => {
              tm.visible = false;
            });
          }
        } else {
          baseMat.color.setHex(0x64748b);
          baseMat.opacity = 0.14;
          link.globeStreamMesh.visible = false;
          link.labStreamMesh.visible = false;
          link.dirConeMesh.visible = false;
          link.trailMeshes.forEach((tm) => {
            tm.visible = false;
          });
        }
      });

      renderer.render(scene, camera);

      // Project 3D coordinates to 2D overlay labels with Screen-Space Anti-Collision Solver
      labelUpdateCounter++;
      if (labelUpdateCounter % 2 === 0 && container) {
        const rectW = container.clientWidth;
        const rectH = container.clientHeight;
        const nextLabels: Record<string, ProjectedLabel> = {};

        current.zones.forEach((zone) => {
          const zObj = zoneGroups[zone.zone_id];
          if (!zObj) return;

          const worldLabelPos = isGlobeMode ? zObj.globeLabelPos : zObj.labLabelPos;
          let isFrontHemisphere = true;
          if (isGlobeMode) {
            _camToNode.copy(camera.position).sub(worldLabelPos).normalize();
            isFrontHemisphere = zObj.surfaceNormal.dot(_camToNode) > -0.12;
          }

          _projVec.copy(worldLabelPos);
          _projVec.project(camera);
          const x = (_projVec.x * 0.5 + 0.5) * rectW;
          const y = (-(_projVec.y * 0.5) + 0.5) * rectH;
          nextLabels[zone.zone_id] = {
            id: zone.zone_id,
            type: 'zone',
            x,
            y,
            visible:
              isFrontHemisphere &&
              _projVec.z < 1 &&
              x > 20 &&
              x < rectW - 20 &&
              y > 20 &&
              y < rectH - 20,
          };
        });

        current.members.forEach((member) => {
          const dNode = deviceNodes[member.device_id];
          if (!dNode) return;

          const worldLabelPos = isGlobeMode ? dNode.globeLabelPos : dNode.labLabelPos;
          let isFrontHemisphere = true;
          if (isGlobeMode) {
            _camToNode.copy(camera.position).sub(worldLabelPos).normalize();
            isFrontHemisphere = dNode.surfaceNormal.dot(_camToNode) > -0.1;
          }

          _projVec.copy(worldLabelPos);
          _projVec.project(camera);
          const x = (_projVec.x * 0.5 + 0.5) * rectW;
          const y = (-(_projVec.y * 0.5) + 0.5) * rectH;
          nextLabels[member.device_id] = {
            id: member.device_id,
            type: 'device',
            x,
            y,
            visible:
              isFrontHemisphere &&
              _projVec.z < 1 &&
              x > 20 &&
              x < rectW - 20 &&
              y > 20 &&
              y < rectH - 20,
          };
        });

        // Prevent co-located same-city labels from overlapping on screen
        resolveScreenLabelOverlaps(nextLabels, rectW, rectH);

        setProjectedLabels(nextLabels);
        setIsZoomedOutClusterView(isCamZoomedOut);
      }

      if (isSoftwareGl) {
        afterDraw(tick);
      } else {
        frameId = requestAnimationFrame(tick);
      }
    }

    frameId = requestAnimationFrame(tick);

    const handleResize = () => {
      if (!container) return;
      const newW = container.clientWidth;
      const newH = container.clientHeight;
      camera.aspect = newW / newH;
      camera.updateProjectionMatrix();
      renderer.setSize(newW, newH);
    };
    window.addEventListener('resize', handleResize);

    return () => {
      isUnmounted = true;
      cancelAnimationFrame(frameId);
      clearTimeout(frameId);
      domElem.removeEventListener('pointerdown', handlePointerDown);
      domElem.removeEventListener('pointermove', handlePointerMove);
      domElem.removeEventListener('pointerup', handlePointerUp);
      window.removeEventListener('resize', handleResize);
      controls.dispose();
      earthTexture.dispose();
      baseStreamTexture.dispose();
      linkPairs.forEach((lp) => lp.flowTexture.dispose());
      renderer.dispose();
    };
  }, []);

  const selectedZone = zones.find((z) => z.zone_id === selectedZoneId);
  const selectedDevice = effectiveMembers.find((m) => m.device_id === selectedDeviceId);

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
      className={`relative w-full h-full min-h-[540px] overflow-hidden select-none border rounded-xl transition-colors duration-500 ${
        resolvedDark
          ? 'bg-[#060B16] border-white/[0.12] shadow-2xl'
          : 'bg-[#E6EFF8] border-slate-200/90 shadow-lg'
      }`}
    >
      {/* ================= CSS FLUID STREAM & CMESH-ORB AMBIENT LAYER ================= */}
      <div
        aria-hidden="true"
        className="pointer-events-none absolute inset-0 overflow-hidden z-0"
      >
        {/* Semi-transparent flowing gradient layer */}
        <div
          className={`absolute inset-0 transition-opacity duration-500 ${
            resolvedDark ? 'cmesh-fluid-gradient-dark' : 'cmesh-fluid-gradient-light'
          }`}
          style={{
            opacity: Math.max(0.25, normalizedIntensity),
            animationPlayState: ambientIntensity === 0 ? 'paused' : 'running',
          }}
        />

        {/* Precision coordinate dot matrix */}
        <div
          className="absolute inset-0 opacity-30"
          style={{
            backgroundImage: resolvedDark
              ? 'radial-gradient(rgba(148, 163, 184, 0.24) 1px, transparent 1px)'
              : 'radial-gradient(rgba(71, 85, 105, 0.18) 1px, transparent 1px)',
            backgroundSize: '26px 26px',
          }}
        />

        {/* cmesh-orb Animated Stream Orbs */}
        <div
          className={`cmesh-orb cmesh-orb-a -top-28 -left-24 w-[440px] h-[440px] blur-3xl ${
            resolvedDark ? 'bg-sky-500/28' : 'bg-sky-400/38'
          }`}
          style={{
            animationPlayState: ambientIntensity === 0 ? 'paused' : 'running',
          }}
        />
        <div
          className={`cmesh-orb cmesh-orb-b top-1/4 -right-24 w-[420px] h-[420px] blur-3xl ${
            resolvedDark ? 'bg-emerald-500/22' : 'bg-teal-300/38'
          }`}
          style={{
            animationPlayState: ambientIntensity === 0 ? 'paused' : 'running',
          }}
        />
        <div
          className={`cmesh-orb cmesh-orb-c -bottom-28 left-1/4 w-[460px] h-[460px] blur-3xl ${
            resolvedDark ? 'bg-indigo-500/25' : 'bg-indigo-300/34'
          }`}
          style={{
            animationPlayState: ambientIntensity === 0 ? 'paused' : 'running',
          }}
        />
        <div
          className={`cmesh-orb cmesh-orb-d top-1/2 left-1/2 -translate-x-1/2 -translate-y-1/2 w-[360px] h-[360px] blur-3xl ${
            resolvedDark ? 'bg-cyan-500/18' : 'bg-cyan-300/30'
          }`}
          style={{
            animationPlayState: ambientIntensity === 0 ? 'paused' : 'running',
          }}
        />
      </div>

      {/* Dedicated WebGL Canvas Container (Transparent over fluid background) */}
      <div
        ref={canvasMountRef}
        className="relative z-10 w-full h-full cursor-grab active:cursor-grabbing"
        role="img"
        aria-label={`三维名册视图：${members.length} 台设备（${members.filter((m) => m.online).length} 台在线），其中锚点 ${members.filter((m) => m.is_anchor).length} 台`}
      />

      {/* The canvas draws its labels with fillText, so they are pixels, not
          DOM nodes: a screen reader (or a headless check) sees nothing. This
          mirror carries the same statement as text — the anchor, the online
          devices and the offline ones still holding a roster seat — so the
          panel can be verified without reading a screenshot
          (doubao-cloud-vm-01, round 58). Visually hidden, not display:none:
          a display:none node is not in the accessibility tree either. */}
      <div className="sr-only" data-testid="cockpit-canvas-mirror">
        <p>
          锚点：{members.filter((m) => m.is_anchor).map((m) => m.display_name || m.device_id).join('、') || '无'}
        </p>
        <p>
          在线设备：
          {members.filter((m) => m.online && !m.is_anchor).map((m) => m.display_name || m.device_id).join('、') || '无'}
        </p>
        <p>
          离线仍在册：
          {members.filter((m) => !m.online).map((m) => m.display_name || m.device_id).join('、') || '无'}
        </p>
      </div>

      {/* Top-Left Global Holographic Earth Hierarchy Breadcrumb & Quick Geolocation Locator */}
      <div className="absolute top-3.5 left-3.5 z-10 flex flex-col gap-1.5 max-w-[calc(100%-180px)]">
        <div className="flex flex-wrap items-center gap-1.5 bg-white/95 border border-slate-200 px-3 py-1.5 rounded-md shadow-xs text-xs">
          {/* 3D View Mode Switcher: Holographic Globe vs Isometric Lab */}
          <div className="flex items-center gap-0.5 bg-slate-100 p-0.5 rounded mr-1">
            <button
              onClick={() => setViewMode('globe')}
              className={`flex items-center gap-1 px-2 py-0.5 rounded text-[11px] font-medium transition-colors cursor-pointer whitespace-nowrap ${
                viewMode === 'globe'
                  ? 'bg-sky-700 text-white shadow-2xs'
                  : 'text-slate-600 hover:text-slate-900'
              }`}
              title="切换至全息地球全球地理视图"
            >
              <Globe className="w-3 h-3" />
              <span>全息地球</span>
            </button>
            <button
              onClick={() => setViewMode('isometric_lab')}
              className={`flex items-center gap-1 px-2 py-0.5 rounded text-[11px] font-medium transition-colors cursor-pointer whitespace-nowrap ${
                viewMode === 'isometric_lab'
                  ? 'bg-sky-700 text-white shadow-2xs'
                  : 'text-slate-600 hover:text-slate-900'
              }`}
              title="切换至等距机房沙盘视图"
            >
              <Layers className="w-3 h-3" />
              <span>等距沙盘</span>
            </button>
          </div>

          <button
            onClick={() => {
              onSelectZone(null);
              onSelectDevice(null);
            }}
            className={`flex items-center gap-1 font-medium transition-colors cursor-pointer ${
              !selectedZoneId && !selectedDeviceId
                ? 'text-sky-700 font-semibold'
                : 'text-slate-600 hover:text-slate-900'
            }`}
          >
            <span>全球总览 ({effectiveMembers.length} 台节点)</span>
          </button>

          {selectedZone && (
            <>
              <ChevronRight className="w-3.5 h-3.5 text-slate-400" />
              <button
                onClick={() => onSelectDevice(null)}
                className={`flex items-center gap-1 font-medium transition-colors cursor-pointer ${
                  selectedZoneId && !selectedDeviceId
                    ? 'text-sky-700 font-semibold'
                    : 'text-slate-600 hover:text-slate-900'
                }`}
              >
                <Server className="w-3.5 h-3.5 text-sky-600" />
                <span>{selectedZone.zone_name}</span>
              </button>
            </>
          )}

          {selectedDevice && (
            <>
              <ChevronRight className="w-3.5 h-3.5 text-slate-400" />
              <span className="flex items-center gap-1 text-sky-700 font-semibold font-mono">
                <Box className="w-3.5 h-3.5 text-sky-600" />
                <span>{selectedDevice.display_name}</span>
                <span className="text-[10px] text-slate-500 font-normal">
                  [{selectedDevice.geo_city} · {selectedDevice.geo_coord_label}]
                </span>
              </span>
            </>
          )}

          {(selectedZoneId || selectedDeviceId) && (
            <button
              onClick={() => {
                if (selectedDeviceId) {
                  onSelectDevice(null);
                } else {
                  onSelectZone(null);
                }
              }}
              className="ml-1 pl-2 border-l border-slate-200 flex items-center gap-1 text-slate-600 hover:text-sky-700 font-medium cursor-pointer"
              title="点击返回上一级视图（也可直接点击 3D 空白处返回上级）"
            >
              <ArrowLeft className="w-3 h-3" />
              <span>返回上级</span>
            </button>
          )}
        </div>

        {/* 1-Click Global Geolocation Node Bar */}
        <div className="flex flex-wrap items-center gap-1 bg-white/90 border border-slate-200/90 px-2.5 py-1 rounded-md shadow-2xs text-[11px]">
          <span className="flex items-center gap-1 text-slate-500 font-medium mr-1">
            <MapPin className="w-3 h-3 text-sky-600" />
            <span>全球节点定位:</span>
          </span>
          {effectiveMembers.map((m) => {
            const isSel = selectedDeviceId === m.device_id;
            const dotTone = !m.online
              ? 'text-slate-400'
              : m.is_anchor
              ? 'text-sky-600'
              : m.endpoints.length === 0 || m.transport === 'relay'
              ? 'text-amber-600'
              : 'text-emerald-600';

            return (
              <button
                key={m.device_id}
                onMouseEnter={() => setHoveredEntityId(m.device_id)}
                onMouseLeave={() =>
                  setHoveredEntityId((prev) => (prev === m.device_id ? null : prev))
                }
                onClick={() => {
                  onSelectZone(m.zone_id);
                  onSelectDevice(m.device_id);
                }}
                className={`flex items-center gap-1 px-2 py-0.5 rounded font-mono transition-colors cursor-pointer whitespace-nowrap ${
                  isSel
                    ? 'bg-slate-900 text-white font-semibold'
                    : 'hover:bg-slate-100 text-slate-700'
                }`}
                title={`点击定位至 ${m.geo_city} (${m.geo_coord_label}) — ${m.display_name}`}
              >
                <span className={isSel ? 'text-sky-400' : dotTone}>
                  {m.is_anchor ? '◈' : m.online ? '●' : '○'}
                </span>
                <span>{m.geo_city.split(' ')[0]}</span>
                <span className="opacity-65 text-[10px]">({m.display_name})</span>
              </button>
            );
          })}
        </div>
      </div>

      {/* Top-Right Status & 3D Archetype Legend */}
      <div className="hidden sm:flex absolute top-3.5 right-3.5 z-20 items-center gap-1.5 bg-white/80 hover:bg-white border border-slate-200/80 px-2.5 py-1 rounded-md shadow-2xs text-xs transition-all">
        {[
          {
            glyph: '◈',
            tone: 'text-sky-600',
            title: '锚点（地理位置未登记）',
            desc: '锚点角色来自实时名册；地理位置、运行时和资源信息未采集。',
          },
          {
            glyph: '●',
            tone: 'text-emerald-600',
            title: '在线直连（公告端点）',
            desc: '实时名册报告在线且存在端点；实际连接质量以 CLI 探测为准。',
          },
          {
            glyph: '◆',
            tone: 'text-amber-600',
            title: '在线但无端点',
            desc: '成员在线但当前没有公告端点；不推断 NAT 地理或中继路径。',
          },
          {
            glyph: '○',
            tone: 'text-slate-500',
            title: '当前离线（仍在名册）',
            desc: '离线不等于被移除；最后在线时间来自名册，地理和运行时信息未采集。',
          },
        ].map((item, idx) => (
          <div key={idx} className="relative group flex items-center">
            <span
              className={`${item.tone} font-mono font-bold text-sm px-1.5 py-0.5 rounded hover:bg-slate-100 cursor-help select-none`}
            >
              {item.glyph}
            </span>
            <div className="pointer-events-none opacity-0 group-hover:opacity-100 transition-opacity duration-150 absolute right-0 top-full mt-2 w-68 p-2.5 bg-slate-900 text-white rounded-md shadow-lg z-30 text-left">
              <div className="font-semibold text-xs flex items-center gap-1.5">
                <span className={item.tone}>{item.glyph}</span>
                <span>{item.title}</span>
              </div>
              <div className="text-[11px] text-slate-300 mt-1 leading-relaxed font-normal">
                {item.desc}
              </div>
            </div>
          </div>
        ))}
      </div>

      {/* Geolocation Target Lock Telemetry HUD */}
      {selectedDevice && (
        <div className="absolute bottom-16 left-3.5 z-20 bg-slate-900/95 text-white border border-sky-500/60 px-3.5 py-2.5 rounded-md shadow-md text-xs max-w-xs">
          <div className="flex items-center justify-between gap-3">
            <div className="flex items-center gap-1.5 text-sky-400 font-mono font-semibold">
              <Crosshair className="w-3.5 h-3.5 shrink-0" />
              <span>已锁定地理坐标 · {selectedDevice.geo_city}</span>
            </div>
            <span className="font-mono text-[10px] text-slate-400 tabular-nums">
              {selectedDevice.geo_coord_label}
            </span>
          </div>
          <div className="mt-1 font-mono text-xs font-bold text-white flex items-center justify-between">
            <span>{selectedDevice.display_name}</span>
            <span className="text-[10px] font-normal text-sky-300">{selectedDevice.alias}</span>
          </div>
          <div className="mt-1 text-[11px] text-slate-300 leading-snug">
            {selectedDevice.active_comm_summary}
          </div>
        </div>
      )}

      {/* Active Communication Toast Banner in 3D Viewport */}
      {activeCommEvent && (
        <div className="absolute top-22 left-3.5 z-20 flex items-center gap-2 bg-sky-950 text-white px-3.5 py-2 rounded-md shadow-md text-xs font-mono">
          <Radio className="w-3.5 h-3.5 text-sky-400 shrink-0" />
          <span>{activeCommEvent.label}</span>
        </div>
      )}

      {/* Semantic 3D Globe Surface Geographic Annotations & Smart Regional Cluster Ring */}
      {showLabels && (
        <div className="absolute inset-0 pointer-events-none overflow-hidden">
          {/* Regional Zone Headers OR Smart Regional Cluster Ring ("区域聚合圆环") on the Globe */}
          {zones.map((zone) => {
            const proj = projectedLabels[zone.zone_id];
            if (!proj || !proj.visible) return null;
            const isZoneSelected = selectedZoneId === zone.zone_id;
            const isZoneHovered = hoveredEntityId === zone.zone_id;
            const isZoneExpanded = isZoneSelected || isZoneHovered;
            const isDimmedByOtherSelection =
              Boolean(selectedZoneId || selectedDeviceId) && !isZoneSelected;
            const zoneMembers = effectiveMembers.filter((m) => m.zone_id === zone.zone_id);
            const zoneOnline = zoneMembers.filter((m) => m.online).length;
            const hasSelectedMemberInZone = zoneMembers.some(
              (m) => m.device_id === selectedDeviceId
            );

            // Check if this zone has co-located same-city devices that should merge into a Regional Cluster Ring
            const isMultiCityZone =
              zoneMembers.length > 1 &&
              zoneMembers.every(
                (m) =>
                  Math.hypot(m.geo_lat - zoneMembers[0].geo_lat, m.geo_lng - zoneMembers[0].geo_lng) <
                  2.5
              );

            const isClusterRingCollapsed =
              viewMode === 'globe' &&
              isZoomedOutClusterView &&
              isMultiCityZone &&
              !isZoneSelected &&
              !hasSelectedMemberInZone;

            if (selectedDeviceId && selectedZoneId !== zone.zone_id) return null;

            // Render Interactive "区域聚合圆环" (Regional Cluster Ring Icon with numeric badge) when zoomed out
            if (isClusterRingCollapsed) {
              const hasAnchor = zoneMembers.some((m) => m.is_anchor);
              const allOffline = zoneOnline === 0;
              const ringBorderColor = allOffline
                ? 'border-slate-400 text-slate-200'
                : isCached
                ? 'border-amber-400 text-amber-200'
                : hasAnchor
                ? 'border-sky-400 text-sky-200'
                : 'border-emerald-400 text-emerald-200';

              const coreBgColor = allOffline
                ? 'bg-slate-800/95'
                : isCached
                ? 'bg-amber-950/95'
                : hasAnchor
                ? 'bg-sky-950/95'
                : 'bg-emerald-950/95';

              const cityShortName =
                zoneMembers[0]?.geo_city.split(' ')[0] ||
                zone.geo_coord_label.split('·')[1]?.trim() ||
                zone.zone_code;

              return (
                <div
                  key={zone.zone_id}
                  style={{
                    transform: `translate3d(${Math.round(proj.x)}px, ${Math.round(proj.y)}px, 0) translate(-50%, -50%)`,
                  }}
                  className="absolute top-0 left-0 z-30 pointer-events-auto"
                >
                  <button
                    onMouseEnter={() => setHoveredEntityId(zone.zone_id)}
                    onMouseLeave={() =>
                      setHoveredEntityId((prev) => (prev === zone.zone_id ? null : prev))
                    }
                    onClick={() => {
                      onSelectZone(zone.zone_id);
                      onSelectDevice(null);
                    }}
                    className="group relative flex flex-col items-center cursor-pointer focus:outline-none"
                    title={`区域聚合圆环：${cityShortName} 共有 ${zoneMembers.length} 台设备 (${zoneOnline} 台在线) · 点击或滚轮放大散开展示具体节点`}
                  >
                    {/* Concentric Animated Regional Cluster Ring Icon ("区域聚合圆环") */}
                    <div className="relative flex items-center justify-center w-12 h-12">
                      {/* Outer breathing pulse ring */}
                      <span
                        className={`absolute inset-0 rounded-full border-2 opacity-65 animate-ping ${
                          allOffline
                            ? 'border-slate-400'
                            : isCached
                            ? 'border-amber-400'
                            : hasAnchor
                            ? 'border-sky-400'
                            : 'border-emerald-400'
                        }`}
                        style={{ animationDuration: '2.4s' }}
                      />
                      {/* Middle segmented orbital ring */}
                      <span
                        className={`absolute inset-1 rounded-full border border-dashed transition-transform duration-500 group-hover:rotate-90 group-hover:scale-110 ${ringBorderColor}`}
                      />
                      {/* Inner solid core with numeric device count */}
                      <div
                        className={`relative flex flex-col items-center justify-center w-8 h-8 rounded-full border-2 shadow-lg transition-transform duration-200 group-hover:scale-110 ${coreBgColor} ${ringBorderColor}`}
                      >
                        <span className="font-mono text-xs font-extrabold leading-none text-white tabular-nums">
                          {zoneMembers.length}
                        </span>
                        <span className="text-[8px] font-mono leading-none opacity-80 mt-0.5">
                          台
                        </span>
                      </div>
                    </div>

                    {/* Compact Regional Cluster Pill Caption */}
                    <div className="mt-1 px-2 py-0.5 rounded bg-slate-900/92 border border-sky-400/50 text-white shadow-md flex items-center gap-1 whitespace-nowrap">
                      <span className="w-1.5 h-1.5 rounded-full bg-sky-400" />
                      <span className="text-[10px] font-semibold">{cityShortName}</span>
                      <span className="text-[10px] font-mono text-sky-300 tabular-nums">
                        {zoneOnline}/{zoneMembers.length}在线
                      </span>
                    </div>

                    {/* Expanded Hover Preview Card listing the aggregated servers */}
                    {isZoneHovered && (
                      <div className="mt-1.5 px-2.5 py-2 rounded-md bg-slate-950/95 border border-sky-400/70 text-white shadow-xl text-left min-w-[190px]">
                        <div className="text-[10px] font-mono text-sky-300 font-semibold flex items-center justify-between">
                          <span>◎ 区域聚合圆环 · {cityShortName}</span>
                          <span>点击或放大散开</span>
                        </div>
                        <div className="mt-1 space-y-0.5 border-t border-white/10 pt-1">
                          {zoneMembers.map((m) => (
                            <div
                              key={m.device_id}
                              className="flex items-center justify-between gap-2 text-[10px] font-mono"
                            >
                              <span className="text-slate-200 truncate">{m.display_name}</span>
                              <span
                                className={
                                  !m.online
                                    ? 'text-slate-400'
                                    : m.is_anchor
                                    ? 'text-sky-400'
                                    : m.transport === 'relay' || m.endpoints.length === 0
                                    ? 'text-amber-400'
                                    : 'text-emerald-400'
                                }
                              >
                                {m.is_anchor ? '锚点' : m.online ? '在线' : '离线'}
                              </span>
                            </div>
                          ))}
                        </div>
                      </div>
                    )}
                  </button>
                </div>
              );
            }

            return (
              <div
                key={zone.zone_id}
                style={{
                  transform: `translate3d(${Math.round(proj.x)}px, ${Math.round(proj.y)}px, 0) translate(-50%, -100%)`,
                }}
                className={`absolute top-0 left-0 pointer-events-auto transition-opacity duration-150 ${
                  isZoneExpanded
                    ? 'z-20 opacity-100'
                    : isDimmedByOtherSelection
                    ? 'z-10 opacity-20 hover:opacity-100'
                    : 'z-10 opacity-45 hover:opacity-100'
                }`}
              >
                <div className="flex items-center gap-1">
                  <button
                    onMouseEnter={() => setHoveredEntityId(zone.zone_id)}
                    onMouseLeave={() =>
                      setHoveredEntityId((prev) => (prev === zone.zone_id ? null : prev))
                    }
                    onClick={() => {
                      onSelectZone(zone.zone_id);
                      onSelectDevice(null);
                    }}
                    className={`group text-left rounded transition-all cursor-pointer ${
                      isZoneExpanded
                        ? 'px-2.5 py-1.5 bg-sky-950/95 text-white border border-sky-500 shadow-sm'
                        : 'px-1.5 py-0.5 bg-white/45 text-slate-700 border border-slate-300/45'
                    }`}
                  >
                    {isZoneExpanded ? (
                      <>
                        <div className="flex items-center gap-2 whitespace-nowrap">
                          <span className="font-mono text-[10px] font-semibold text-sky-300">
                            {zone.geo_coord_label}
                          </span>
                          <span className="text-[10px] opacity-60">·</span>
                          <span className="font-mono text-[10px] tabular-nums text-emerald-300">
                            {zoneOnline}/{zoneMembers.length} 台在线 (已散开)
                          </span>
                        </div>
                        <div className="text-xs font-semibold whitespace-nowrap mt-0.5 flex items-center gap-1">
                          <span>{zone.zone_name}</span>
                        </div>
                      </>
                    ) : (
                      <div className="flex items-center gap-1 whitespace-nowrap text-[10px] font-mono">
                        <span>{zone.geo_coord_label.split('·')[1]?.trim() || zone.zone_code}</span>
                        <span className="opacity-60">·</span>
                        <span className="tabular-nums">
                          {zoneOnline}/{zoneMembers.length}台
                        </span>
                      </div>
                    )}
                  </button>

                  {isMultiCityZone && isZoneSelected && (
                    <button
                      onClick={(e) => {
                        e.stopPropagation();
                        handleSetCameraPreset('global');
                      }}
                      className="px-2 py-1.5 rounded bg-slate-900/95 hover:bg-slate-800 text-sky-300 border border-sky-500/60 text-[10px] font-mono whitespace-nowrap shadow-sm cursor-pointer"
                      title="缩回全球视角并重新合并为区域聚合圆环"
                    >
                      ◎ 收起聚合
                    </button>
                  )}
                </div>
              </div>
            );
          })}

          {/* Individual Server Geographic Pin Annotations (Hidden for clusters that are currently collapsed into a Regional Cluster Ring) */}
          {effectiveMembers.map((member) => {
            const proj = projectedLabels[member.device_id];
            if (!proj || !proj.visible) return null;

            // Check if this member is currently collapsed into a Regional Cluster Ring
            const sameCityPeers = effectiveMembers.filter(
              (other) =>
                Math.hypot(other.geo_lat - member.geo_lat, other.geo_lng - member.geo_lng) < 2.5
            );
            const anyPeerSelected = sameCityPeers.some((p) => p.device_id === selectedDeviceId);
            const isCollapsedInClusterRing =
              viewMode === 'globe' &&
              isZoomedOutClusterView &&
              sameCityPeers.length > 1 &&
              selectedZoneId !== member.zone_id &&
              !anyPeerSelected;

            if (isCollapsedInClusterRing) return null;

            const isSelected = selectedDeviceId === member.device_id;
            const isHovered = hoveredEntityId === member.device_id;
            const isExpanded = isSelected || isHovered;
            const isDimmedByOtherDevice = Boolean(selectedDeviceId) && !isSelected;
            const hasEndpoints = member.endpoints.length > 0;

            let glyph = '●';
            let statusTone = 'text-emerald-600';
            let shortState = '直连在线';

            if (!member.online) {
              glyph = '○';
              statusTone = 'text-slate-500';
              shortState = '离线(在册)';
            } else if (!hasEndpoints) {
              glyph = '◆';
              statusTone = 'text-amber-600';
              shortState = '无端点';
            } else if (member.transport === 'relay') {
              glyph = '⇄';
              statusTone = 'text-amber-600';
              shortState = '锚点中继';
            } else if (member.is_anchor) {
              glyph = '◈';
              statusTone = 'text-sky-600';
              shortState = '核心锚点';
            }

            return (
              <div
                key={member.device_id}
                style={{
                  transform: `translate3d(${Math.round(proj.x)}px, ${Math.round(proj.y)}px, 0) translate(-50%, -100%)`,
                }}
                className={`absolute top-0 left-0 pointer-events-auto transition-opacity duration-150 ${
                  isExpanded
                    ? 'z-30 opacity-100'
                    : isDimmedByOtherDevice
                    ? 'z-10 opacity-20 hover:opacity-100'
                    : 'z-10 opacity-65 hover:opacity-100'
                }`}
              >
                <div className="flex flex-col items-center">
                  <button
                    onMouseEnter={() => setHoveredEntityId(member.device_id)}
                    onMouseLeave={() =>
                      setHoveredEntityId((prev) => (prev === member.device_id ? null : prev))
                    }
                    onClick={() => {
                      onSelectZone(member.zone_id);
                      onSelectDevice(member.device_id);
                    }}
                    className={`rounded text-left transition-all cursor-pointer whitespace-nowrap ${
                      isExpanded
                        ? 'px-3 py-1.5 bg-slate-900/95 text-white border border-sky-500 shadow-md scale-105'
                        : 'px-1.5 py-0.5 bg-white/60 hover:bg-white/95 text-slate-800 border border-slate-300/60 shadow-2xs'
                    }`}
                  >
                    {isExpanded ? (
                      <div className="space-y-0.5">
                        <div className="flex items-center gap-1.5 text-xs font-mono">
                          <span className="text-sky-400 font-bold">{glyph}</span>
                          <span className="font-bold text-white">{member.display_name}</span>
                          <span className="text-[10px] text-sky-300">· {shortState}</span>
                        </div>
                        <div className="text-[10px] text-slate-300 flex items-center gap-1.5 font-mono">
                          <span className="text-sky-200 font-semibold">📍 {member.geo_city}</span>
                          <span>({member.geo_coord_label})</span>
                          <span>·</span>
                          <span>
                            {hasEndpoints ? member.endpoints[0] : '无入站端点 (endpoints: [])'}
                          </span>
                        </div>
                      </div>
                    ) : (
                      <div className="flex items-center gap-1 text-[10px] font-mono">
                        <span className={`${statusTone} font-bold`}>{glyph}</span>
                        <span className="font-semibold text-slate-800">
                          {member.geo_city.split(' ')[0]}
                        </span>
                        <span className="text-slate-500">{member.display_name}</span>
                      </div>
                    )}
                  </button>
                  <div
                    className={`w-px h-2 ${
                      isExpanded ? 'bg-sky-500' : 'bg-slate-400/50'
                    }`}
                  />
                </div>
              </div>
            );
          })}
        </div>
      )}

      {/* Collapsible Bottom Control Dock */}
      <div
        className="absolute left-1/2 bottom-3 z-10 -translate-x-1/2 transition-transform duration-200"
        style={{
          maxWidth: 'min(960px, 96vw)',
        }}
      >
        <div className="flex flex-col items-center">
          <button
            onClick={() => setDockCollapsed(!dockCollapsed)}
            className="px-3 py-1 bg-white border border-b-0 border-slate-200 rounded-t-md text-[11px] font-medium text-slate-600 hover:text-slate-900 flex items-center gap-1 shadow-2xs cursor-pointer"
          >
            {dockCollapsed ? (
              <>
                <Maximize2 className="w-3 h-3" />
                <span>▴ 展开全息地球控制栏</span>
              </>
            ) : (
              <>
                <Minimize2 className="w-3 h-3" />
                <span>▾ 收起控制栏 (纯净视图)</span>
              </>
            )}
          </button>

          {!dockCollapsed && (
            <div className="flex flex-wrap items-center gap-2.5 bg-white/96 border border-slate-200 px-3.5 py-2 rounded-lg shadow-sm text-xs text-slate-700">
              {/* Quick Planetary Region Camera Fly-To Presets */}
              <div className="flex items-center gap-1 bg-slate-100 p-0.5 rounded-md">
                <button
                  onClick={() => handleSetCameraPreset('global')}
                  className={`px-2.5 py-1 rounded text-xs font-medium transition-colors cursor-pointer whitespace-nowrap ${
                    cameraPreset === 'global'
                      ? 'bg-white text-slate-900 shadow-2xs'
                      : 'text-slate-600 hover:text-slate-900'
                  }`}
                >
                  全球总览
                </button>
                <button
                  onClick={() => handleSetCameraPreset('apac')}
                  className={`px-2.5 py-1 rounded text-xs font-medium transition-colors cursor-pointer whitespace-nowrap ${
                    cameraPreset === 'apac'
                      ? 'bg-white text-slate-900 shadow-2xs'
                      : 'text-slate-600 hover:text-slate-900'
                  }`}
                >
                  亚太区 (港/日/新)
                </button>
                <button
                  onClick={() => handleSetCameraPreset('euro')}
                  className={`px-2.5 py-1 rounded text-xs font-medium transition-colors cursor-pointer whitespace-nowrap ${
                    cameraPreset === 'euro'
                      ? 'bg-white text-slate-900 shadow-2xs'
                      : 'text-slate-600 hover:text-slate-900'
                  }`}
                >
                  欧洲区 (德/瑞)
                </button>
              </div>

              <span className="text-slate-300">|</span>

              {/* 3D Node Size Multiplier Slider */}
              <label className="flex items-center gap-1.5 text-xs text-slate-600 whitespace-nowrap">
                <span>节点体积</span>
                <input
                  type="range"
                  min={0.6}
                  max={1.6}
                  step={0.1}
                  value={nodeScaleMultiplier}
                  onChange={(e) => setNodeScaleMultiplier(parseFloat(e.target.value))}
                  className="w-16 accent-sky-600 cursor-pointer"
                />
                <span className="font-mono text-[11px] text-slate-500 tabular-nums w-8">
                  {Math.round(nodeScaleMultiplier * 100)}%
                </span>
              </label>

              <span className="text-slate-300">|</span>

              {/* Exploded NAT Glass & Regional Shield Slider */}
              <label className="flex items-center gap-1.5 text-xs text-slate-600 whitespace-nowrap">
                <span>护罩拆解</span>
                <input
                  type="range"
                  min={0}
                  max={1}
                  step={0.05}
                  value={explodeFactor}
                  onChange={(e) => setExplodeFactor(parseFloat(e.target.value))}
                  className="w-14 accent-sky-600 cursor-pointer"
                />
                <span className="font-mono text-[11px] text-slate-500 tabular-nums w-8">
                  {Math.round(explodeFactor * 100)}%
                </span>
              </label>

              {onChangeAmbientIntensity && (
                <>
                  <span className="text-slate-300">|</span>
                  <label
                    className="flex items-center gap-1.5 text-xs text-slate-600 whitespace-nowrap"
                    title="调节 3D 驾驶舱与全局毛玻璃背景的 cmesh-orb 流动态流光强度"
                  >
                    <span>流光强度</span>
                    <input
                      type="range"
                      min={0}
                      max={100}
                      step={5}
                      value={ambientIntensity}
                      onChange={(e) => onChangeAmbientIntensity(parseInt(e.target.value, 10))}
                      className="w-14 accent-sky-600 cursor-pointer"
                    />
                    <span className="font-mono text-[11px] text-slate-500 tabular-nums w-8">
                      {ambientIntensity}%
                    </span>
                  </label>
                </>
              )}

              <span className="text-slate-300">|</span>

              {/* Toggle Auto-Rotate Globe */}
              {viewMode === 'globe' && (
                <button
                  onClick={() => setAutoRotateGlobe(!autoRotateGlobe)}
                  className="flex items-center gap-1 px-2 py-1 rounded hover:bg-slate-100 text-slate-700 cursor-pointer whitespace-nowrap"
                >
                  <Compass className="w-3.5 h-3.5 text-sky-600" />
                  <span>{autoRotateGlobe ? '地球自转: 开' : '地球自转: 关'}</span>
                </button>
              )}

              {/* Toggle Floating Labels */}
              <button
                onClick={() => setShowLabels(!showLabels)}
                className="flex items-center gap-1 px-2 py-1 rounded hover:bg-slate-100 text-slate-700 cursor-pointer whitespace-nowrap"
              >
                {showLabels ? <Eye className="w-3.5 h-3.5" /> : <EyeOff className="w-3.5 h-3.5" />}
                <span>{showLabels ? '隐藏标签' : '显示标签'}</span>
              </button>

              {/* Toggle Communication Flow Orbs */}
              <button
                onClick={() => setShowCommPulses(!showCommPulses)}
                className="flex items-center gap-1 px-2 py-1 rounded hover:bg-slate-100 text-slate-700 cursor-pointer whitespace-nowrap"
              >
                <Radio className="w-3.5 h-3.5 text-sky-600" />
                <span>{showCommPulses ? '飞线脉冲: 开' : '飞线脉冲: 静默'}</span>
              </button>

              {/* Reset Selection */}
              {(selectedZoneId || selectedDeviceId) && (
                <button
                  onClick={() => handleSetCameraPreset('global')}
                  className="flex items-center gap-1 px-2.5 py-1 bg-sky-50 text-sky-700 hover:bg-sky-100 rounded font-medium cursor-pointer whitespace-nowrap"
                >
                  <RotateCcw className="w-3 h-3" />
                  <span>重置全球视角</span>
                </button>
              )}
            </div>
          )}
        </div>
      </div>
    </div>
  );
};
