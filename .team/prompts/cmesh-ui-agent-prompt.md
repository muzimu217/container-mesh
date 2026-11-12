# container-mesh UI design prompt

Use the `ui-design-agent-kit` repository, especially the `anatomy` skill, as the design reference. Apply its principle of turning an abstract technical system into a precise, understandable interactive instrument: an isometric technical object, clear painter's order, stable parts, live readouts driven by the same model, calm motion, and evidence-first interaction.

## Product

Design a web dashboard for **container-mesh / cmesh**. It is a self-hosted, user-space private mesh for containers and machines. Nodes can discover one another and exchange messages, files, commands, and latency measurements without root, CAP_NET_ADMIN, TUN, or public inbound ports. The dashboard is the network owner's cockpit.

## Primary user questions

The first viewport must answer:

- How many devices belong to this network?
- How many are reachable right now?
- Which node is the discovery anchor?
- Which connections are direct, relayed, or unavailable?
- Why is a node unavailable, and when was it last seen?

## Design direction

Create a **minimal 3D technical cockpit**:

- Use an isometric or lightly perspective network instrument, not a decorative galaxy or cyberpunk scene.
- Use a calm dark graphite or deep blue surface with one cyan/teal network accent and restrained semantic colors.
- Keep the 3D model legible: a central mesh core, a small number of orbiting device nodes, thin connection paths, and a subtle ground/grid plane.
- Make the 3D scene useful for selecting a device and understanding the current network, not merely ornamental.
- Use physical-instrument cues inspired by Anatomy: a base plane, clean rails/orbits, stable node bodies, small labels, a live readout, and precise status markings.
- Animation must be slow and purposeful: connection pulses or small node movement only when data changes; respect reduced motion.
- Avoid neon overload, rainbow nodes, glassmorphism, giant gradients, noisy particle fields, and game-like HUD decoration.

## Layout

Use a desktop-first responsive layout:

1. Top bar
   - cmesh wordmark
   - current network name / account short ID
   - live connection indicator
   - roster version and last data time
   - read-only badge for v1

2. Main cockpit
   - large 3D/isometric mesh view on the left
   - compact network summary and selected-device readout on the right
   - no nested cards; keep the main scene dominant and the detail area quiet

3. Network summary
   - prominent `online / members` value
   - anchor status
   - transport mode summary
   - latest roster change

4. Selected device readout
   - device name or ID
   - online/offline state
   - endpoints
   - last seen
   - connection type
   - available future actions, clearly marked as planned if not implemented

5. Footer/status strip
   - data source
   - refresh cadence
   - last successful refresh
   - cached-data warning when the discovery node is unavailable

## State model

Do not conflate these states:

- membership: the device belongs to the roster
- liveness: the device is currently reachable
- announcement: the device has announced endpoints
- discovery connection: the cockpit can or cannot reach the discovery node

An offline member remains in the roster and should be gray/neutral, not red. A missing endpoint should be shown as a separate condition. A stale cached view must be clearly labeled.

## Data contract for prototype

Use mock data only in the prototype, but structure it as if it came from `GET /api/mesh`:

```json
{
  "account_id": "7ehkextbmg6mgi7n",
  "version": 42,
  "generated_at": "2026-11-13T08:00:05Z",
  "summary": { "members": 5, "online": 4 },
  "members": [
    {
      "device_id": "anchor-175",
      "display_name": "发现节点",
      "online": true,
      "endpoints": ["175.24.139.148:18090"],
      "last_seen": "2026-11-13T08:00:04Z",
      "is_anchor": true,
      "transport": "relay"
    },
    {
      "device_id": "runner-02",
      "display_name": "runner-02",
      "online": true,
      "endpoints": ["10.0.0.2:7000"],
      "last_seen": "2026-11-13T07:59:59Z",
      "is_anchor": false,
      "transport": "direct"
    },
    {
      "device_id": "agent-03",
      "display_name": "agent-03",
      "online": true,
      "endpoints": ["203.0.113.4:7000"],
      "last_seen": "2026-11-13T07:59:56Z",
      "is_anchor": false,
      "transport": "direct"
    },
    {
      "device_id": "worker-04",
      "display_name": "worker-04",
      "online": true,
      "endpoints": [],
      "last_seen": "2026-11-13T07:59:40Z",
      "is_anchor": false,
      "transport": "pending"
    },
    {
      "device_id": "storage-01",
      "display_name": "storage-01",
      "online": false,
      "endpoints": [],
      "last_seen": "2026-11-12T19:03:11Z",
      "is_anchor": false,
      "transport": "unavailable"
    }
  ]
}
```

## Prototype interactions

The prototype should demonstrate:

- selecting a node in the 3D scene or device list updates the readout
- toggling a device offline updates the `online / members` value and node appearance
- simulating discovery-node loss changes the top status to a warning, preserves the last snapshot, and labels the data cached
- selecting a node with no endpoint explains that it belongs to the network but is not currently dialable
- reduced-motion mode avoids continuous orbit animation

## Technical output

- Deliver a standalone HTML file with inline CSS, SVG or CSS 3D, and vanilla JavaScript.
- No CDN, remote font, image, framework, or external data request.
- Keep the prototype self-contained and easy to open with a local static server.
- Clearly label mock/demo controls as prototype-only. Do not present mock numbers as production data.
- Include a compact design note in the HTML or adjacent markdown explaining the visual system and what should become real API data later.

## Acceptance

- The first viewport reads as a network instrument, not a marketing page.
- The 3D view helps explain device relationships and supports selection.
- The dashboard remains usable at 1280px desktop and 390px mobile widths.
- Online, offline, no-endpoint, cached, direct, and relay states are distinguishable without relying on color alone.
- The page remains legible and useful when animation is disabled.
- No visual element claims a capability that the current cmesh implementation does not provide.
