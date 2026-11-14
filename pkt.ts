// pkt-line framing for the git smart-HTTP protocol (gitprotocol-pack / gitprotocol-v2).
//
// A data packet is 4 lowercase-hex ASCII bytes giving the total length including the header,
// then the payload. Control packets: flush "0000", delim "0001", response-end "0002". The largest
// packet is 65520 bytes (git's LARGE_PACKET_MAX, header included), so a payload is at most 65516.
// Side-band multiplexing (side-band-64k) prefixes each packet's payload with a band byte: 1 = data,
// 2 = progress, 3 = fatal error (this server never sends band 3).

/** The largest pkt-line, header included. */
export const PKT_LINE_MAX = 65520;
/** The largest payload a pkt-line carries. */
export const PKT_MAX_PAYLOAD = PKT_LINE_MAX - 4;

/** The flush packet, "0000". */
export const FLUSH = Buffer.from('0000', 'ascii');
/** The delim packet, "0001". */
export const DELIM = Buffer.from('0001', 'ascii');
/** The response-end packet, "0002". */
export const RESPONSE_END = Buffer.from('0002', 'ascii');

/** One decoded packet: a data payload, or a control marker. */
export type Packet =
  | { kind: 'data'; payload: Buffer }
  | { kind: 'flush' }
  | { kind: 'delim' }
  | { kind: 'response-end' };

/** Encode one payload as a pkt-line. Throws when the payload does not fit. */
export function pktLine(payload: Buffer | string): Buffer {
  const data = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  if (data.length > PKT_MAX_PAYLOAD) throw new Error(`pkt-line payload of ${data.length} bytes exceeds ${PKT_MAX_PAYLOAD}`);
  const header = (data.length + 4).toString(16).padStart(4, '0');
  return Buffer.concat([Buffer.from(header, 'ascii'), data]);
}

/** Encode several payloads, each its own pkt-line, concatenated. */
export function pktLines(...payloads: readonly (Buffer | string)[]): Buffer {
  return Buffer.concat(payloads.map((payload) => pktLine(payload)));
}

/**
 * Decode a whole buffer of pkt-lines. `allowTrailing` returns the bytes after the last complete
 * packet (the receive-pack request carries a raw packfile after its flush). A malformed header,
 * an over-long packet or a truncated tail throws.
 */
export function decodePktLines(
  bytes: Buffer,
  options: { allowTrailing?: boolean } = {},
): { packets: Packet[]; trailing: Buffer } {
  const packets: Packet[] = [];
  let offset = 0;
  while (offset < bytes.length) {
    if (offset + 4 > bytes.length) throw new Error('pktline-truncated: short header at end of buffer');
    const header = bytes.subarray(offset, offset + 4).toString('ascii');
    if (!/^[0-9a-f]{4}$/.test(header)) {
      if (options.allowTrailing) break;
      throw new Error(`pktline-header: bad length ${JSON.stringify(header)}`);
    }
    const length = parseInt(header, 16);
    if (length === 0) {
      packets.push({ kind: 'flush' });
      offset += 4;
      continue;
    }
    if (length === 1) {
      packets.push({ kind: 'delim' });
      offset += 4;
      continue;
    }
    if (length === 2) {
      packets.push({ kind: 'response-end' });
      offset += 4;
      continue;
    }
    if (length === 3) throw new Error('pktline-header: length 0003 is not a packet');
    if (length > PKT_LINE_MAX) throw new Error(`pktline-too-long: ${length} bytes`);
    if (offset + length > bytes.length) throw new Error('pktline-truncated: packet overruns the buffer');
    packets.push({ kind: 'data', payload: Buffer.from(bytes.subarray(offset + 4, offset + length)) });
    offset += length;
  }
  const trailing = Buffer.from(bytes.subarray(offset));
  if (!options.allowTrailing && trailing.length > 0) throw new Error('pktline-after-end: trailing bytes after the packets');
  return { packets, trailing };
}

/** The data payloads of decoded packets, as UTF-8 strings, control packets dropped. */
export function packetLines(packets: readonly Packet[]): string[] {
  const lines: string[] = [];
  for (const packet of packets) {
    if (packet.kind === 'data') lines.push(packet.payload.toString('utf8'));
  }
  return lines;
}

/**
 * Frame arbitrary bytes as side-band band-1 packets, each at most PKT_MAX_PAYLOAD - 1 bytes of
 * payload (one byte is the band marker). Progress text goes on band 2 the same way.
 */
export function sideband(data: Buffer, band: 1 | 2): Buffer[] {
  const chunks: Buffer[] = [];
  const max = PKT_MAX_PAYLOAD - 1;
  for (let offset = 0; offset < data.length; offset += max) {
    const slice = data.subarray(offset, Math.min(offset + max, data.length));
    chunks.push(pktLine(Buffer.concat([Buffer.from([band]), slice])));
  }
  return chunks;
}

/** Parse a capability list ("key=value key2 ...") into a map; a bare key maps to ''. */
export function parseCapabilities(text: string): Map<string, string> {
  const capabilities = new Map<string, string>();
  for (const token of text.split(' ').filter((part) => part.length > 0)) {
    const at = token.indexOf('=');
    if (at === -1) capabilities.set(token, '');
    else capabilities.set(token.slice(0, at), token.slice(at + 1));
  }
  return capabilities;
}

/** Render a capability map back to its wire form, keys in insertion order. */
export function renderCapabilities(capabilities: ReadonlyMap<string, string>): string {
  const parts: string[] = [];
  for (const [key, value] of capabilities) parts.push(value === '' ? key : `${key}=${value}`);
  return parts.join(' ');
}
