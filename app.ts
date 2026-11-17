// The forge's HTTP server: one node:http server, routed by path prefix to the four surfaces.
//
//   GET  /healthz                      boot probe, no auth
//   /api/v3/...                        REST v3            (src/rest/)
//   POST /api/graphql                  GraphQL v4         (src/graphql/)
//   POST /mcp                          Model Context Protocol, streamable HTTP (src/mcp/)
//   /<owner>/<repo>.git/...            git smart HTTP     (src/git/)
//   anything else                      404 {"message":"Not Found"}

import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { handleGit } from './src/git/smart-http.ts';
import { handleGraphql } from './src/graphql/endpoint.ts';
import { HttpError, sendError, sendJson } from './src/http.ts';
import { handleMcp } from './src/mcp/endpoint.ts';
import { handleRest } from './src/rest/router.ts';

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
  const query = new URLSearchParams(url.length > path.length ? url.slice(path.length + 1) : '');
  if (path === '/api/v3' || path.startsWith('/api/v3/')) {
    await handleRest(req, res, path, query);
    return;
  }
  if (path === '/mcp' || path === '/mcp/') {
    await handleMcp(req, res);
    return;
  }
  if (path === '/api/graphql' || path === '/graphql') {
    await handleGraphql(req, res);
    return;
  }
  if (path.endsWith('/info/refs') || path.endsWith('/git-upload-pack') || path.endsWith('/git-receive-pack')) {
    if (await handleGit(req, res, path, query)) return;
  }
  void method;
  req.resume();
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
