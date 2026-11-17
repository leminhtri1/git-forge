// Process-wide settings, read once from the environment.
//
// The forge serves exactly one account: the login the bearer token names. The trial suite's token is
// opaque, so the login is configuration: FORGE_LOGIN, else the suite's fixed scenario viewer.
// Time is never read from the wall clock for anything the forge stores: every timestamp derives from
// MAKEFUN_TRIAL_NOW (the trial's "now"), so two runs of the same trial build byte-identical objects.

export interface Identity {
  readonly login: string;
  readonly id: number;
  readonly name: string;
  readonly email: string;
}

function parseNow(raw: string | undefined): number {
  if (raw !== undefined && raw.trim() !== '') {
    const trimmed = raw.trim();
    if (/^\d+$/.test(trimmed)) {
      const n = Number(trimmed);
      return n > 1e11 ? Math.floor(n / 1000) : n; // milliseconds or seconds
    }
    const parsed = Date.parse(trimmed);
    if (Number.isFinite(parsed)) return Math.floor(parsed / 1000);
  }
  return 1_767_225_600; // 2026-01-01T00:00:00Z: a fixed fallback, never the clock
}

const login = (process.env.FORGE_LOGIN ?? '').trim() || 'adaiversen';

export const config = {
  /** The bearer token every API request must present; empty means "accept any bearer token". */
  token: (process.env.MAKEFUN_SHOP_TOKEN ?? '').trim(),
  viewer: {
    login,
    id: 1000,
    name: (process.env.FORGE_NAME ?? '').trim() || login,
    email: `${login}@users.noreply.forge.local`,
  } as Identity,
  /** The trial's "now", in epoch seconds. */
  nowSec: parseNow(process.env.MAKEFUN_TRIAL_NOW),
  version: 'git-forge 1.0.0',
  defaultBranch: 'main',
};

/** An ISO-8601 timestamp `offset` seconds after the trial's now (GitHub's format, no millis). */
export function isoAt(offsetSec = 0): string {
  return new Date((config.nowSec + offsetSec) * 1000).toISOString().replace(/\.\d{3}Z$/, 'Z');
}
