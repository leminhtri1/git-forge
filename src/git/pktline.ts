// pkt-line framing (gitprotocol-common): a 4-hex-digit length prefix that counts itself, plus the three
// special packets 0000 (flush), 0001 (delimiter) and 0002 (response-end).

export const FLUSH = Buffer.from('0000', 'ascii');
export const DELIM = Buffer.from('0001', 'ascii');

/** The largest payload one packet may carry: 65520 bytes total, minus the 4-byte header. */
export const MAX_PAYLOAD = 65516;

export type Packet =
  | { readonly kind: 'data'; readonly data: Buffer }
  | { readonly kind: 'flush' }
  | { readonly kind: 'delim' }
  | { readonly kind: 'end' };

export class PktLineError extends Error {}

/** One data packet around `payload` (a string is UTF-8 encoded as-is). */
export function pkt(payload: string | Buffer): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  if (body.length > MAX_PAYLOAD) throw new PktLineError(`pkt-line payload of ${body.length} bytes is too long`);
  const header = Buffer.from((body.length + 4).toString(16).padStart(4, '0'), 'ascii');
  return Buffer.concat([header, body]);
}

/** Frame `data` on side-band channel `band`, split into packets that respect the 64k limit. */
export function sideband(band: 1 | 2 | 3, data: Buffer, out: Buffer[]): void {
  const chunk = MAX_PAYLOAD - 1;
  for (let at = 0; at < data.length; at += chunk) {
    const piece = data.subarray(at, Math.min(data.length, at + chunk));
    const header = Buffer.allocUnsafe(5);
    header.write((piece.length + 5).toString(16).padStart(4, '0'), 0, 'ascii');
    header[4] = band;
    out.push(header, piece);
  }
}

/**
 * Decode packets from the start of `buf`. Returns the packets and the offset where decoding stopped:
 * `stopAtFlush` ends after the first flush (receive-pack's command list is followed by raw pack bytes).
 */
export function decodePackets(buf: Buffer, stopAtFlush = false): { packets: Packet[]; end: number } {
  const packets: Packet[] = [];
  let at = 0;
  while (at < buf.length) {
    if (buf.length - at < 4) throw new PktLineError('truncated pkt-line length');
    const lenText = buf.toString('ascii', at, at + 4);
    if (!/^[0-9a-fA-F]{4}$/.test(lenText)) throw new PktLineError(`bad pkt-line length ${JSON.stringify(lenText)}`);
    const len = parseInt(lenText, 16);
    if (len === 0) {
      packets.push({ kind: 'flush' });
      at += 4;
      if (stopAtFlush) break;
      continue;
    }
    if (len === 1) {
      packets.push({ kind: 'delim' });
      at += 4;
      continue;
    }
    if (len === 2) {
      packets.push({ kind: 'end' });
      at += 4;
      continue;
    }
    if (len < 4) throw new PktLineError(`invalid pkt-line length ${len}`);
    if (at + len > buf.length) throw new PktLineError('truncated pkt-line payload');
    packets.push({ kind: 'data', data: buf.subarray(at + 4, at + len) });
    at += len;
  }
  return { packets, end: at };
}

/** A data packet's payload as text without its trailing newline. */
export function lineOf(packet: Packet): string | null {
  if (packet.kind !== 'data') return null;
  let text = packet.data.toString('utf8');
  if (text.endsWith('\n')) text = text.slice(0, -1);
  return text;
}
