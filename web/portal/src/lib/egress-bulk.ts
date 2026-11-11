export type BulkRow = Record<string, string | number | Record<string, string>>;
export const bulkFields = {
 ips: ['cidr', 'protocol', 'port'],
 domains: ['host', 'port'],
 services: ['name', 'namespace', 'podSelector', 'protocol', 'port'],
} as const;
export type BulkGroup = keyof typeof bulkFields;
export function parseBulk(group: BulkGroup, text: string): BulkRow[] {
 const result: BulkRow[] = [];
 for (const [index, raw] of text.split(/\r?\n/).entries()) {
  if (!raw.trim()) continue;
  const cells = raw.includes('\t') ? raw.trim().split('\t').map(s => s.trim()) : raw.trim().split(/\s+/);
  const fields = bulkFields[group];
  if (cells.length !== fields.length || cells.some(s => !s)) throw new Error(`第 ${index + 1} 行：需要 ${fields.length} 列，请使用 Tab 或空格分隔`);
  const row: BulkRow = Object.fromEntries(fields.map((field, i) => [field, cells[i]]));
  const port = String(row.port);
  if (!/^\d+$/.test(port) || Number(port) < 1 || Number(port) > 65535) throw new Error(`第 ${index + 1} 行：端口须为 1–65535 的整数`);
  row.port = Number(port);
  if ('protocol' in row) {
   row.protocol = String(row.protocol).toUpperCase();
   if (!['TCP', 'UDP'].includes(row.protocol)) throw new Error(`第 ${index + 1} 行：协议须为 TCP 或 UDP`);
  }
  if (group === 'domains') row.host = String(row.host).toLowerCase();
  if (group === 'services') {
   const keys = new Set<string>();
   for (const pair of String(row.podSelector).split(',')) {
    const at = pair.indexOf('=');
    const key = pair.slice(0, at);
    if (at < 1 || at === pair.length - 1 || keys.has(key)) throw new Error(`第 ${index + 1} 行：Pod 标签须为不重复的 key=value，逗号分隔`);
    keys.add(key);
   }
  }
  result.push(row);
 }
 if (!result.length) throw new Error('请先粘贴规则');
 return result;
}
export function formatBulk(group: BulkGroup, rows: BulkRow[]): string {
 return rows.map(row => bulkFields[group].map(field => String(row[field] ?? '')).join('\t')).join('\n');
}
