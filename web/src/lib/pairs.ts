// Parses "channel=C0123, user=U1" into {channel: "C0123", user: "U1"}.
// Throws on anything that isn't key=value, so a typo can't silently turn a
// filter into "match everything".
export function parsePairs(text: string): Record<string, string> {
  const out: Record<string, string> = {}
  for (const raw of text.split(',')) {
    const part = raw.trim()
    if (!part) continue
    const eq = part.indexOf('=')
    const key = part.slice(0, eq).trim()
    const value = part.slice(eq + 1).trim()
    if (eq <= 0 || !key || !value) throw new Error(`"${part}" isn't key=value`)
    out[key] = value
  }
  return out
}
