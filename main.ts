// git-forge entrypoint: start the forge on an ephemeral port, announce the port to the trial env,
// then keep serving until the env reports the suite is done.
//
//   MAKEFUN_ENV_URL   the env origin; POST {port} to /register, poll GET /done
//   MAKEFUN_SHOP_TOKEN the bearer token every API call must carry (read by src/config.ts)
//
// Without MAKEFUN_ENV_URL the forge runs standalone (handy for local poking): it listens on
// FORGE_PORT (or an ephemeral port) and never exits on its own.
//
// The forge itself is the Go build in bin/ (go/, built for linux/amd64, and windows/amd64 for local
// testing): this process starts it, reads the port it prints and registers that port. When there is
// no binary for this platform, it cannot be run, or it prints no port within 3 s, the Node forge in
// app.ts serves instead. FORGE_NODE=1 forces the Node forge.

import { spawn, type ChildProcess } from 'node:child_process';
import { accessSync, chmodSync, constants as fsConstants, copyFileSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
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

interface Forge {
  readonly port: number;
  close(): Promise<void>;
}

/** The native forge for this platform, or null when there is none to run here. */
function nativeBinary(): string | null {
  let name: string;
  if (process.platform === 'linux' && process.arch === 'x64') name = 'forge-linux-amd64';
  else if (process.platform === 'win32' && process.arch === 'x64') name = 'forge-windows.exe';
  else return null;
  const path = fileURLToPath(new URL(`./bin/${name}`, import.meta.url));
  if (!existsSync(path)) return null;
  if (process.platform === 'win32') return path;
  try {
    accessSync(path, fsConstants.X_OK);
    return path;
  } catch {
    // /bot is read-only and the file lost its exec bit: run a private executable copy instead.
    try {
      const copy = join(tmpdir(), `forge-linux-amd64-${process.pid}`);
      copyFileSync(path, copy);
      chmodSync(copy, 0o755);
      return copy;
    } catch {
      return null;
    }
  }
}

/** Start the Go forge and read the "PORT <n>" line it prints; null if it does not come up within 3 s. */
async function startNative(port: number): Promise<Forge | null> {
  const binary = nativeBinary();
  if (binary === null) return null;
  let child: ChildProcess;
  try {
    child = spawn(binary, [], {
      env: { ...process.env, FORGE_PORT: String(port) },
      stdio: ['ignore', 'pipe', 'inherit'],
    });
  } catch {
    return null;
  }
  const bound = await new Promise<number | null>((resolve) => {
    let text = '';
    const timer = setTimeout(() => resolve(null), 3000);
    child.once('error', () => {
      clearTimeout(timer);
      resolve(null);
    });
    child.once('exit', () => {
      clearTimeout(timer);
      resolve(null);
    });
    child.stdout!.on('data', (chunk: Buffer) => {
      text += chunk.toString('utf8');
      const match = /PORT (\d+)/.exec(text);
      if (match !== null) {
        clearTimeout(timer);
        resolve(Number(match[1]));
      }
    });
  });
  if (bound === null) {
    child.kill();
    return null;
  }
  child.stdout!.on('data', (chunk: Buffer) => process.stdout.write(chunk));
  child.on('exit', (code, signal) => process.stderr.write(`[forge] native forge exited (${code ?? signal})\n`));
  return {
    port: bound,
    close: () =>
      new Promise<void>((done) => {
        if (child.exitCode !== null) return done();
        child.once('exit', () => done());
        child.kill();
        setTimeout(done, 2000).unref();
      }),
  };
}

async function run(): Promise<void> {
  const wantedPort = envUrl === '' ? Number(process.env.FORGE_PORT ?? 0) : 0;
  let forge: Forge | null = null;
  if (process.env.FORGE_NODE !== '1') forge = await startNative(wantedPort);
  if (forge === null) forge = await startForge(wantedPort);
  else process.stderr.write(`[forge] native forge running (${process.platform}/${process.arch})\n`);
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
