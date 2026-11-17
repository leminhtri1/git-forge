// The forge's HTTP server: one node:http server, routed by path prefix to the four surfaces.
//
//   GET  /healthz                      boot probe, no auth
//   /api/v3/...                        REST v3            (src/rest/)
//   POST /api/graphql                  GraphQL v4         (src/graphql/)
//   POST /mcp                          Model Context Protocol, streamable HTTP (src/mcp/)
//   /<owner>/<repo>.git/...            git smart HTTP     (src/git/)
//   anything else                      404 {"message":"Not Found"}

import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { config } from './src/config.ts';
import { HttpError, sendError, sendJson } from './src/http.ts';

export interface RunningForge {
  readonly port: number;
  close(): Promise<void>;
}

function pathOf(url: string): string {
  const q = url.indexOf('?');
  return q < 0 ? url : url.slice(0, q);
}

async function route(req: IncomingMessage, res: ServerResponse): Promise<void> {
  const url = req.url ?? '/';
  const path = pathOf(url);
  const method = req.method ?? 'GET';

  if (path === '/healthz') {
    sendJson(res, 200, { ok: true });
    return;
  }
  if (path === '/api/v3' || path === '/api/v3/') {
    sendJson(res, 200, { current_user_url: '/api/v3/user', meta_url: '/api/v3/meta' }, { 'x-oauth-scopes': 'repo, read:org' });
    return;
  }
  void method;
  sendError(res, 404, 'Not Found');
}

export function createForgeServer() {
  const server = createServer((req, res) => {
    route(req, res).catch((err: unknown) => {
      if (res.headersSent) {
        res.destroy();
        return;
      }
      if (err instanceof HttpError) sendError(res, err.status, err.message, err.extra);
      else {
        process.stderr.write(`[forge] ${req.method} ${req.url}: ${err instanceof Error ? err.stack : String(err)}\n`);
        sendError(res, 500, 'Internal Server Error');
      }
    });
  });
  server.keepAliveTimeout = 120_000;
  server.headersTimeout = 125_000;
  server.requestTimeout = 0;
  return server;
}

export function startForge(port = 0): Promise<RunningForge> {
  const server = createForgeServer();
  return new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(port, () => {
      const address = server.address();
      if (address === null || typeof address === 'string') {
        reject(new Error('the forge did not bind a TCP port'));
        return;
      }
      resolve({
        port: address.port,
        close: () =>
          new Promise<void>((done) => {
            server.close(() => done());
            server.closeAllConnections();
          }),
      });
    });
  });
}

void config;
