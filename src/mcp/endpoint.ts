// POST /mcp: the Model Context Protocol over the Streamable HTTP transport, JSON-RPC 2.0.
//
// Both protocol eras are served: revision 2026-07-28 (no handshake; `server/discover` describes the
// server and every request names its revision in `_meta`) and the handshake revisions (`initialize` then
// `notifications/initialized`). No session is kept: every request carries the bearer token and stands on
// its own, so concurrent requests never interfere. Answers are application/json.

import type { Req as IncomingMessage, Res as ServerResponse } from '../http.ts';
import { config } from '../config.ts';
import { HttpError, isAuthorized, nextRequestId, originOf, readBody } from '../http.ts';
import { findTool, InvalidArguments, TOOL_LIST } from './tools.ts';

const LATEST = '2026-07-28';
const SUPPORTED = [LATEST, '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];
const HANDSHAKE_DEFAULT = '2025-11-25';
const SERVER_INFO = { name: 'git-forge', title: 'git-forge MCP server', version: '1.0.0' };
const CAPABILITIES = { tools: { listChanged: false } };
const INSTRUCTIONS = 'Tools for a GitHub-compatible forge: repositories, issues and pull requests of the authenticated user.';

type JsonRpcId = string | number | null;

interface RpcResponse {
  jsonrpc: '2.0';
  id: JsonRpcId;
  result?: unknown;
  error?: { code: number; message: string; data?: unknown };
}

function send(res: ServerResponse, status: number, body: unknown, extra: Record<string, string> = {}): void {
  const text = body === undefined ? '' : JSON.stringify(body);
  const headers: Record<string, string | number> = { 'x-github-request-id': nextRequestId(), 'content-length': Buffer.byteLength(text), ...extra };
  if (body !== undefined) headers['content-type'] = 'application/json';
  res.writeHead(status, headers);
  res.end(text);
}

const rpcError = (id: JsonRpcId, code: number, message: string, data?: unknown): RpcResponse => ({ jsonrpc: '2.0', id, error: data === undefined ? { code, message } : { code, message, data } });

function toolResult(value: unknown, isError: boolean) {
  const textValue = typeof value === 'string' ? value : JSON.stringify(value);
  const result: Record<string, unknown> = { content: [{ type: 'text', text: textValue }], isError };
  if (!isError && value !== null && typeof value === 'object' && !Array.isArray(value)) result.structuredContent = value;
  return result;
}

function callTool(params: Record<string, unknown>, origin: string, id: JsonRpcId): RpcResponse {
  const name = params.name;
  if (typeof name !== 'string' || name === '') return rpcError(id, -32602, 'Invalid params: tools/call needs a tool name');
  const tool = findTool(name);
  if (tool === undefined) return rpcError(id, -32602, `Unknown tool: ${name}`);
  const args = params.arguments ?? {};
  if (args === null || typeof args !== 'object' || Array.isArray(args)) return rpcError(id, -32602, 'Invalid params: arguments must be an object');
  try {
    const value = tool.run(args as Record<string, unknown>, { api: `${origin}/api/v3`, web: origin });
    return { jsonrpc: '2.0', id, result: toolResult(value, false) };
  } catch (err) {
    if (err instanceof InvalidArguments) return rpcError(id, -32602, `Invalid params: ${err.message}`);
    if (err instanceof HttpError) {
      const details = Array.isArray(err.extra?.errors) ? ` (${(err.extra!.errors as { message?: string }[]).map((e) => e.message).filter(Boolean).join('; ')})` : '';
      return { jsonrpc: '2.0', id, result: toolResult(`${err.status} ${err.message}${details}`, true) };
    }
    return { jsonrpc: '2.0', id, result: toolResult(err instanceof Error ? err.message : String(err), true) };
  }
}

/** Dispatch one JSON-RPC message; null for a notification (no answer). */
function dispatch(message: unknown, origin: string): RpcResponse | null {
  if (message === null || typeof message !== 'object' || Array.isArray(message)) return rpcError(null, -32600, 'Invalid Request');
  const msg = message as Record<string, unknown>;
  const hasId = 'id' in msg && (typeof msg.id === 'string' || typeof msg.id === 'number');
  const id: JsonRpcId = hasId ? (msg.id as string | number) : null;
  if (msg.jsonrpc !== '2.0' || typeof msg.method !== 'string') {
    if (!hasId && ('result' in msg || 'error' in msg)) return null; // a response from the client: nothing to say
    return rpcError(id, -32600, 'Invalid Request');
  }
  if (!hasId) return null; // notifications (notifications/initialized, cancelled, ...) need no answer
  const params = msg.params !== null && typeof msg.params === 'object' && !Array.isArray(msg.params) ? (msg.params as Record<string, unknown>) : {};
  switch (msg.method) {
    case 'server/discover':
      return {
        jsonrpc: '2.0',
        id,
        result: {
          supportedVersions: SUPPORTED,
          capabilities: CAPABILITIES,
          serverInfo: SERVER_INFO,
          instructions: INSTRUCTIONS,
          _meta: { 'io.modelcontextprotocol/serverInfo': SERVER_INFO },
        },
      };
    case 'initialize': {
      const asked = typeof params.protocolVersion === 'string' ? params.protocolVersion : HANDSHAKE_DEFAULT;
      const protocolVersion = SUPPORTED.includes(asked) ? asked : HANDSHAKE_DEFAULT;
      return { jsonrpc: '2.0', id, result: { protocolVersion, capabilities: CAPABILITIES, serverInfo: SERVER_INFO, instructions: INSTRUCTIONS } };
    }
    case 'ping':
      return { jsonrpc: '2.0', id, result: {} };
    case 'tools/list':
      return { jsonrpc: '2.0', id, result: { tools: TOOL_LIST } };
    case 'tools/call':
      return callTool(params, origin, id);
    case 'resources/list':
      return { jsonrpc: '2.0', id, result: { resources: [] } };
    case 'resources/templates/list':
      return { jsonrpc: '2.0', id, result: { resourceTemplates: [] } };
    case 'prompts/list':
      return { jsonrpc: '2.0', id, result: { prompts: [] } };
    case 'logging/setLevel':
      return { jsonrpc: '2.0', id, result: {} };
    default:
      return rpcError(id, -32601, `Method not found: ${msg.method}`);
  }
}

export async function handleMcp(req: IncomingMessage, res: ServerResponse): Promise<void> {
  if (!isAuthorized(req)) {
    req.resume();
    send(res, 401, rpcError(null, -32001, 'Unauthorized: a bearer token is required'), { 'www-authenticate': 'Bearer realm="git-forge"' });
    return;
  }
  if (req.method !== 'POST') {
    req.resume();
    send(res, 405, rpcError(null, -32000, 'Method not allowed: POST JSON-RPC messages to this endpoint'), { allow: 'POST' });
    return;
  }
  const raw = await readBody(req, 8 * 1024 * 1024);
  let message: unknown;
  try {
    message = JSON.parse(raw.toString('utf8'));
  } catch {
    send(res, 400, rpcError(null, -32700, 'Parse error'));
    return;
  }
  const origin = originOf(req);
  if (Array.isArray(message)) {
    if (message.length === 0) {
      send(res, 400, rpcError(null, -32600, 'Invalid Request: empty batch'));
      return;
    }
    const answers = message.map((m) => dispatch(m, origin)).filter((a): a is RpcResponse => a !== null);
    if (answers.length === 0) send(res, 202, undefined);
    else send(res, 200, answers);
    return;
  }
  const answer = dispatch(message, origin);
  if (answer === null) {
    send(res, 202, undefined);
    return;
  }
  send(res, answer.error?.code === -32600 ? 400 : 200, answer);
}

void config;
void HttpError;
