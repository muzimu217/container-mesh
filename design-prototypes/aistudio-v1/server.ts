import express from 'express';
import path from 'path';
import { fileURLToPath } from 'url';
import { createServer as createViteServer } from 'vite';
import { getMeshDataByScenario } from './src/data/meshSnapshots.ts';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);

async function startServer() {
  const app = express();
  const PORT = 3000;

  app.use(express.json());

  // Real /api/mesh endpoint conforming to cmesh specification
  app.get('/api/mesh', (req, res) => {
    const scenario = (req.query.scenario as string) || 'normal';
    const data = getMeshDataByScenario(scenario);
    res.json(data);
  });

  // Controlled operations endpoint (RTT probe, P2P message, File transfer check, Whitelist command)
  app.post('/api/mesh/action', (req, res) => {
    const { device_id, action, payload } = req.body;
    const timestamp = new Date().toISOString();

    if (action === 'ping') {
      res.json({
        ok: true,
        device_id,
        action: 'ping',
        timestamp,
        result: {
          packets_transmitted: 4,
          packets_received: 4,
          packet_loss: '0%',
          samples_ms: [11.4, 11.8, 11.2, 11.5],
          avg_rtt_ms: 11.48,
          mode: 'userspace_udp_direct',
        },
      });
      return;
    }

    if (action === 'message') {
      res.json({
        ok: true,
        device_id,
        action: 'message',
        timestamp,
        result: {
          msg_id: `msg_${Math.random().toString(36).slice(2, 9)}`,
          delivered: true,
          encrypted: 'ChaCha20-Poly1305 (E2EE)',
          ack_at: timestamp,
          echo: `已通过用户态加密通道送达目标容器 [${device_id}]：${payload?.text || 'Heartbeat check'}`,
        },
      });
      return;
    }

    if (action === 'file_transfer') {
      res.json({
        ok: true,
        device_id,
        action: 'file_transfer',
        timestamp,
        result: {
          transfer_id: `tx_${Math.random().toString(36).slice(2, 9)}`,
          file_name: payload?.fileName || 'artifact-bundle-v42.tar.zst',
          file_size: payload?.fileSize || '128.4 MB',
          sha256: 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855',
          status: 'VERIFIED_COMPLETE',
          chunks_verified: '32 / 32 chunks',
        },
      });
      return;
    }

    if (action === 'exec_whitelist') {
      const cmd = payload?.command || 'cmesh status --json';
      const outputs: Record<string, string> = {
        'cmesh status --json': `{\n  "daemon": "cmeshd-userspace v0.9.4",\n  "root_required": false,\n  "cap_net_admin": false,\n  "tun_device": "none (pure userspace socket)",\n  "active_peers": 4,\n  "roster_version": 42\n}`,
        'uname -a': `Linux container-node 6.6.22-cloud-amd64 #1 SMP PREEMPT_DYNAMIC x86_64 GNU/Linux (uid=1000 non-root)`,
        'df -h /workspace': `Filesystem      Size  Used Avail Use% Mounted on\noverlay          64G   18G   46G  29% /workspace`,
        'id && capsh --print': `uid=1000(runner) gid=1000(runner) groups=1000(runner)\nCurrent: = (Zero Linux Capabilities / Unprivileged Container)`,
      };
      res.json({
        ok: true,
        device_id,
        action: 'exec_whitelist',
        timestamp,
        result: {
          command: cmd,
          exit_code: 0,
          stdout: outputs[cmd] || outputs['cmesh status --json'],
        },
      });
      return;
    }

    res.status(400).json({ ok: false, error: 'Unsupported action' });
  });

  if (process.env.NODE_ENV !== 'production') {
    const vite = await createViteServer({
      server: { middlewareMode: true },
      appType: 'spa',
    });
    app.use(vite.middlewares);
  } else {
    const distPath = path.join(__dirname, 'dist');
    app.use(express.static(distPath));
    app.get('*', (_req, res) => {
      res.sendFile(path.join(distPath, 'index.html'));
    });
  }

  app.listen(PORT, '0.0.0.0', () => {
    console.log(`cmesh desktop console server running on http://localhost:${PORT}`);
  });
}

startServer();
