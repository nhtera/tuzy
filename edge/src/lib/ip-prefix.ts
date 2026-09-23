/**
 * Rate-limit keys by network prefix: IPv4 /32, IPv6 /64 (a single host usually owns a whole /64,
 * so per-address IPv6 limits are trivially bypassed). `ipv4Slash24` is used by phase 7.
 */

function expandIPv6(ip: string): string[] | null {
  let addr = ip.toLowerCase();
  const zone = addr.indexOf("%");
  if (zone !== -1) addr = addr.slice(0, zone);
  // IPv4-mapped tail (::ffff:1.2.3.4)
  const v4 = /(\d+\.\d+\.\d+\.\d+)$/.exec(addr);
  if (v4) {
    const p = v4[1]!.split(".").map(Number);
    if (p.some((n) => n > 255)) return null;
    addr = addr.slice(0, -v4[1]!.length) + ((p[0]! << 8) | p[1]!).toString(16) + ":" + ((p[2]! << 8) | p[3]!).toString(16);
  }
  const halves = addr.split("::");
  if (halves.length > 2) return null;
  const head = halves[0] ? halves[0].split(":") : [];
  const tail = halves.length === 2 && halves[1] ? halves[1].split(":") : [];
  const fill = halves.length === 2 ? 8 - head.length - tail.length : 0;
  if (fill < 0) return null;
  const groups = [...head, ...Array<string>(fill).fill("0"), ...tail];
  if (groups.length !== 8 || groups.some((g) => !/^[0-9a-f]{1,4}$/.test(g))) return null;
  return groups.map((g) => g.replace(/^0+(?=.)/, ""));
}

function isIPv4(ip: string): boolean {
  const p = ip.split(".");
  return p.length === 4 && p.every((x) => /^\d{1,3}$/.test(x) && Number(x) <= 255);
}

export function ipPrefix(ip: string | null | undefined): string {
  let s = (ip ?? "").trim();
  const mapped = /^::ffff:(\d+\.\d+\.\d+\.\d+)$/i.exec(s);
  if (mapped) s = mapped[1]!; // IPv4-mapped IPv6 is really an IPv4 client
  if (isIPv4(s)) return `${s}/32`;
  const g = expandIPv6(s);
  if (g) return `${g.slice(0, 4).join(":")}::/64`;
  return "unknown";
}

export function ipv4Slash24(ip: string): string | null {
  if (!isIPv4(ip)) return null;
  return `${ip.split(".").slice(0, 3).join(".")}.0/24`;
}
