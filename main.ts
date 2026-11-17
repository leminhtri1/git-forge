// git-forge entrypoint: start the forge on an ephemeral port, announce the port to the trial env,
// then keep serving until the env reports the suite is done.
//
//   MAKEFUN_ENV_URL   the env origin; POST {port} to /register, poll GET /done
//   MAKEFUN_SHOP_TOKEN the bearer token every API call must carry (read by src/config.ts)
//
// Without MAKEFUN_ENV_URL the forge runs standalone (handy for local poking): it listens on
// FORGE_PORT (or an ephemeral port) and never exits on its own.

import { startForge } from './app.ts';

const envUrl = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');

const wait = (ms: number): Promise<void> => new Promise((done) => setTimeout(done, ms));

/** Back-off between retries: 100ms doubling to a 1.5s ceiling. */
function backoff(attempt: number): number {
  return Math.min(1500, 100 * 2 ** Math.min(attempt, 6));
}

async function announce(port: number): Promise<void> {
  for (let attempt = 0; ; attempt++) {
    try {
      const res = await fetch(`${envUrl}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(20_000),
      });
      await res.arrayBuffer().catch(() => undefined);
      // 200: registered. 409: somebody registered first, which is not ours to fix.
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) throw new Error(`register refused with HTTP ${res.status}`);
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register refused')) throw err;
    }
    await wait(backoff(attempt));
  }
}

async function untilDone(): Promise<void> {
  for (let attempt = 0; ; attempt++) {
    try {
      const res = await fetch(`${envUrl}/done`, { signal: AbortSignal.timeout(20_000) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
        attempt = 0;
      }
    } catch {
      // the env is busy or not up yet: keep polling
    }
    await wait(attempt === 0 ? 500 : backoff(attempt));
  }
}

async function run(): Promise<void> {
  const wantedPort = envUrl === '' ? Number(process.env.FORGE_PORT ?? 0) : 0;
  const forge = await startForge(wantedPort);
  process.stdout.write(`[forge] listening on port ${forge.port}\n`);
  if (envUrl === '') return; // standalone: serve forever
  await announce(forge.port);
  process.stdout.write('[forge] registered with the env\n');
  await untilDone();
  process.stdout.write('[forge] env reports done, shutting down\n');
  await forge.close();
  process.exit(0);
}

run().catch((err) => {
  process.stderr.write(`[forge] fatal: ${err instanceof Error ? err.stack ?? err.message : String(err)}\n`);
  process.exit(1);
});
