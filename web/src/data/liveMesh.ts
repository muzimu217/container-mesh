// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

// Live roster access.
//
// The prototype fetched a mock endpoint and fell back to canned scenarios
// when it failed. In the product there is one source of truth: the
// discovery node's GET /api/mesh. This module owns the two things the
// prototype got wrong:
//
//   - a failed fetch must NOT paint demo data. It keeps the last good
//     roster and marks it stale, because a dashboard that can invent
//     numbers is a dashboard that lies;
//   - the data endpoint is protected by the panel token, so the token is
//     asked for once and kept in this browser.

import { MeshApiResponse } from './meshSnapshots';

const TOKEN_KEY = 'cmesh.panel.token';
export const LIVE_POLL_MS = 5000;

export type LiveState = {
  data: MeshApiResponse | null;
  loading: boolean;
  stale: boolean;
  /** True when the node refused the token; the UI asks for it. */
  needsToken: boolean;
  error: string | null;
};

export function panelToken(): string {
  try {
    return window.localStorage.getItem(TOKEN_KEY) || '';
  } catch {
    return '';
  }
}

export function savePanelToken(value: string): void {
  try {
    window.localStorage.setItem(TOKEN_KEY, value);
  } catch {
    /* private mode: the token simply does not persist */
  }
}

/**
 * Fetch the live roster.
 *
 * `previous` is the last good payload: on failure it is returned with
 * discovery_status flipped to 'disconnected_cached', so the cockpit keeps
 * showing the last known state behind the "node unreachable" banner instead
 * of blanking or inventing one.
 */
export async function fetchLiveRoster(
  previous: MeshApiResponse | null
): Promise<{ data: MeshApiResponse; needsToken: boolean; error: string | null }> {
  const headers: Record<string, string> = {};
  const token = panelToken();
  if (token) headers['X-Panel-Token'] = token;

  try {
    const res = await fetch('/api/mesh', { headers, cache: 'no-store' });
    if (res.status === 401) {
      return {
        data: staleCopy(previous),
        needsToken: true,
        error: '面板令牌未被接受',
      };
    }
    if (!res.ok) {
      throw new Error(`HTTP ${res.status}`);
    }
    const data = (await res.json()) as MeshApiResponse;
    return { data, needsToken: false, error: null };
  } catch (err) {
    return {
      data: staleCopy(previous),
      needsToken: false,
      error: err instanceof Error ? err.message : String(err),
    };
  }
}

/** Mark a roster as cached without inventing anything. */
function staleCopy(previous: MeshApiResponse | null): MeshApiResponse {
  if (!previous) {
    return {
      account_id: '',
      network_name: '',
      roster_version: 0,
      generated_at: '',
      discovery_status: 'disconnected_cached',
      is_cached: true,
      last_sync_at: '',
      retry_in_seconds: Math.round(LIVE_POLL_MS / 1000),
      zones: [],
      members: [],
    };
  }
  return {
    ...previous,
    discovery_status: 'disconnected_cached',
    is_cached: true,
    retry_in_seconds: Math.round(LIVE_POLL_MS / 1000),
  };
}

