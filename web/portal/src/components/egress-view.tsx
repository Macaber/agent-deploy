import { useEffect, useState } from 'react';
import { apiUrl } from '@/lib/api';
import { useApp } from '@/store';
import { parseBulk, formatBulk, type BulkGroup } from '@/lib/egress-bulk';
import { Button } from '@/components/ui/button';
type Replica = { pod: string; state: string; loadedRevision: string; observedAt: string; error: string };
type SyncStatus = { pendingWorkspaces?: string[]; revision: string; proxyState: string; desiredReplicas: number; replicas: Replica[]; policyState: string; migrationState: string };
const stateLabel: Record<string, string> = { loaded: '已加载', syncing: '同步中', failed: '同步失败', unverified: '尚未验证', not_required: '此模式无需代理', pending: '等待 Pod 切换完成', observed: '已观测到 Pod 模式一致（网络策略尚未验证）' };
type Row = Record<string, string | number | Record<string, string>>;
type Mode = 'legacy' | 'allowlist' | 'unrestricted';
const modeLabels: Record<Mode,string> = {legacy:'恢复原有默认策略',allowlist:'启用白名单',unrestricted:'全部出站开放'};
type Config = Record<'ips' | 'domains' | 'services', Row[]> & {mode?: Mode};
const groups: Array<{key: BulkGroup; title: string; fields: string[]; initial: Row}> = [
 { key: 'ips' as const, title: 'IP/CIDR 规则', fields: ['cidr', 'protocol', 'port'], initial: { cidr: '', protocol: 'TCP', port: 443 } },
 { key: 'domains' as const, title: '域名代理规则', fields: ['host', 'port'], initial: { host: '', port: 443 } },
 { key: 'services' as const, title: '必要基础服务规则', fields: ['name', 'namespace', 'podSelector', 'protocol', 'port'], initial: { name: '', namespace: '', podSelector: '', protocol: 'UDP', port: 53 } },
];
const labels: Record<string,string> = { cidr:'IP/CIDR', protocol:'协议 TCP/UDP', port:'端口', host:'完整域名', name:'用途', namespace:'命名空间', podSelector:'Pod 标签（key=value，逗号分隔）' };
async function request(init?: RequestInit) {
 const r = await fetch(apiUrl('api/egress/config'), { credentials:'same-origin', ...init, headers:{'Content-Type':'application/json'} });
 if (r.status === 401) window.dispatchEvent(new CustomEvent('bw:unauthorized'));
 if (!r.ok) throw new Error(r.status === 409 ? '其他管理员已修改配置，请重新加载后合并。' : await r.text());
 return r.json();
}
export function EgressView() {
 const [bulk, setBulk] = useState<Record<BulkGroup, string>>({ips:'', domains:'', services:''});
 const [bulkMessage, setBulkMessage] = useState('');
 const authenticated = useApp(s=>s.auth.authenticated);
 const [syncStatus, setSyncStatus] = useState<SyncStatus | null>(null);
 const [statusError, setStatusError] = useState('');
 useEffect(() => {
  if (!authenticated) { setSyncStatus(null); return; }
  let stopped = false;
  let timer: ReturnType<typeof setTimeout>;
  const controller = new AbortController();
  async function refresh() {
   try {
    const response = await fetch(apiUrl('api/egress/status'), { credentials: 'same-origin', signal: controller.signal });
    if (response.status === 401) window.dispatchEvent(new CustomEvent('bw:unauthorized'));
    if (!response.ok) throw new Error(await response.text());
    const status = await response.json() as SyncStatus;
    if (!stopped) { setSyncStatus(status); setStatusError(''); }
   } catch (e) { if (!stopped) { setSyncStatus(null); setStatusError(String(e)); } }
   finally { if (!stopped) timer = setTimeout(() => void refresh(), 5000); }
  }
  void refresh();
  return () => { stopped = true; clearTimeout(timer); controller.abort(); };
 }, [authenticated]);
 const [config,setConfig]=useState<Config>({ips:[],domains:[],services:[]});
 const [version,setVersion]=useState(''); const [busy,setBusy]=useState(false);
 const [loaded,setLoaded]=useState(false); const [dirty,setDirty]=useState(false);
 const [message,setMessage]=useState('尚未读取'); const [error,setError]=useState('');
 async function load() {
  setBusy(true);setError('');
  try { const d=await request();const c=d.config as Config;
   setConfig({mode:c.mode || (d.state==='unconfigured'?'legacy':'allowlist'),ips:c.ips||[],domains:c.domains||[],services:(c.services||[]).map(r=>({...r,podSelector:Object.entries(r.podSelector||{}).map(([k,v])=>`${k}=${v}`).join(',')}))});
   setVersion(d.resourceVersion);setDirty(d.state==='unconfigured');setLoaded(true);setMessage((d.state==='unconfigured'?'尚未配置':'已保存；尚未确认执行组件加载状态') + ((d.warnings || []).length ? '；迁移前检查：' + d.warnings.join('；') : ''));
  } catch(e){setError(String(e));}finally{setBusy(false);}
 }
 useEffect(()=>{if(authenticated)void load();},[authenticated]);
 async function save(targetMode?: Mode){
  const changed = targetMode !== undefined && targetMode !== (config.mode || 'allowlist');
  if(changed && !window.confirm('切换为“'+modeLabels[targetMode!]+'”将重建所有受影响的运行中 Workspace Pod，中断连接。已有规则会保留。确认继续？')) return;
  setBusy(true);setError('');try{
  const services=config.services.map(r=>{const selector:Record<string,string>={};for(const pair of String(r.podSelector).split(',')){const at=pair.indexOf('=');if(at<1||at===pair.length-1)throw new Error('Pod 标签须为 key=value');const key=pair.slice(0,at).trim();if(Object.hasOwn(selector,key))throw new Error('Pod 标签键重复');selector[key]=pair.slice(at+1).trim();}return {...r,podSelector:selector};});
  await request({method:'PUT',body:JSON.stringify({config:{...config,services,mode:targetMode || config.mode},resourceVersion:version,confirmModeChange:changed})});await load();
 }catch(e){setError(String(e));}finally{setBusy(false);}}
 function appendBulk(group: BulkGroup) {
  try {
   const rows = parseBulk(group, bulk[group]);
   if (config.ips.length + config.domains.length + config.services.length + rows.length > 1000) throw new Error('三类规则合计最多 1000 条');
   const seen = new Set(config[group].map(r => formatBulk(group, [r])));
   for (const row of rows) { const key = formatBulk(group, [row]); if (seen.has(key)) throw new Error('存在重复规则，请检查输入和当前表单'); seen.add(key); }
   setConfig(c => ({...c, [group]: [...c[group], ...rows]})); setDirty(true); setError('');
   setBulk(b => ({...b, [group]: ''})); setBulkMessage(`已追加 ${rows.length} 条，请检查表单后保存；保存时将校验完整规则。`);
  } catch(e) { setError(String(e)); }
 }
 async function pasteBulk(group: BulkGroup) {
  try { const text = await navigator.clipboard.readText(); setBulk(b => ({...b, [group]: text})); setBulkMessage('已粘贴，请追加到表单。'); }
  catch { setBulkMessage('浏览器未允许读取剪贴板，请在输入框内按 Ctrl+V 粘贴。'); }
 }
 async function copyBulk(group: BulkGroup) {
  const text = formatBulk(group, config[group]); setBulk(b => ({...b, [group]: text}));
  try { await navigator.clipboard.writeText(text); setBulkMessage('已复制当前表单规则（包含未保存修改）。'); }
  catch { setBulkMessage('规则已放入输入框，请选中后按 Ctrl+C 复制。'); }
 }
 return <div className="h-full overflow-auto p-6"><div className="mx-auto max-w-5xl space-y-5">
 <h1 className="text-xl font-semibold">全局出站控制</h1>
 <section className="space-y-2 rounded border p-4"><p>已读取的目标模式：{modeLabels[config.mode || 'allowlist']}。模式切换会重建 Pod；同模式修改规则不会重建 Pod。</p><div className="flex flex-wrap gap-2">{(Object.keys(modeLabels) as Mode[]).map(mode=><Button key={mode} disabled={busy||!loaded||dirty||mode===(config.mode||'allowlist')} onClick={()=>void save(mode)}>{modeLabels[mode]}</Button>)}</div><p className="text-xs">请先保存或放弃规则修改再切换模式。保存成功不代表所有 Workspace 已完成切换。</p></section>
 <p className="text-sm">IP/CIDR 规则允许直连，并允许通过代理访问对应 IPv4 地址的 TCP 端口；域名规则允许 HTTP/HTTPS 代理访问。UDP 及 IPv6 规则仅用于直连。代理仍禁止访问受保护地址。目标服务的登录和接口权限仍需满足。</p>
 <p role="status">{dirty?'有未保存修改。':''}{message}。保存成功不代表实际生效。</p>
 <section aria-label="已保存配置的同步状态" className="space-y-2 rounded border p-4">
 <h2 className="font-semibold">已保存配置的同步状态</h2>
 {statusError && <p role="alert">无法确认状态：{statusError}</p>}
 {syncStatus ? <>
 <p>代理：{stateLabel[syncStatus.proxyState] || syncStatus.proxyState} · 期望副本：{syncStatus.desiredReplicas}</p>
 <p className="break-all text-xs">目标版本：{syncStatus.revision}</p>
 <div className="overflow-auto"><table className="w-full text-left text-sm"><thead><tr><th>副本</th><th>状态</th><th>加载版本</th><th>最近报告</th></tr></thead><tbody>
 {syncStatus.replicas.map(replica => <tr key={replica.pod}><td>{replica.pod}</td><td>{stateLabel[replica.state] || replica.state}{replica.error && <p className="text-red-600">{replica.error}</p>}</td><td title={replica.loadedRevision}>{replica.loadedRevision?.slice(0, 12) || '—'}</td><td>{replica.observedAt || '—'}</td></tr>)}
 </tbody></table></div>
 <p>网络策略：{stateLabel[syncStatus.policyState] || syncStatus.policyState}；Workspace 迁移：{stateLabel[syncStatus.migrationState] || syncStatus.migrationState}</p>
 {syncStatus.pendingWorkspaces?.length ? <p className="text-sm">待完成：{syncStatus.pendingWorkspaces.join('、')}</p> : null}
 <p className="text-xs text-muted-foreground">代理副本加载完成不代表所有 Workspace 已迁移，也不代表集群网络策略已完成验收。</p>
 </> : !statusError && <p>正在读取副本状态</p>}
 </section>
 {error&&<p role="alert" className="text-red-600">{error}</p>}
 {bulkMessage && <p role="status">{bulkMessage}</p>}
 {groups.map(g=><section key={g.key} className="space-y-3 rounded border p-4"><h2 className="font-semibold">{g.title}</h2>
 <details className="rounded border p-3"><summary className="cursor-pointer text-sm">批量粘贴 / 复制规则</summary>
 <p className="my-2 text-xs">每行一条，按以下顺序用 Tab 或空格分隔：{g.fields.map(f=>labels[f]).join(' → ')}。支持从 Excel 复制。用途含空格时请使用 Tab 分隔；标签内部不加空格。追加仅修改表单，保存后才提交。</p>
 <textarea aria-label={`${g.title}批量输入`} className="min-h-32 w-full rounded border bg-background p-2 font-mono text-sm" disabled={busy||!loaded} value={bulk[g.key]} onChange={e=>setBulk(b=>({...b,[g.key]:e.target.value}))} placeholder={g.key==='ips'?'192.0.2.1/32 TCP 443':g.key==='domains'?'api.example.com 443':'DNS kube-system k8s-app=kube-dns UDP 53'}/>
 <div className="mt-2 flex flex-wrap gap-2"><Button variant="outline" disabled={busy||!loaded} onClick={()=>void pasteBulk(g.key)}>粘贴</Button><Button variant="outline" disabled={busy||!loaded||!bulk[g.key].trim()} onClick={()=>appendBulk(g.key)}>追加到表单</Button><Button variant="outline" disabled={busy||!loaded||!config[g.key].length} onClick={()=>void copyBulk(g.key)}>复制当前规则</Button></div>
 </details>
 {config[g.key].map((r,i)=><div key={i} className="flex flex-wrap items-end gap-2">{g.fields.map(f=><label key={f} className="flex min-w-32 flex-1 flex-col text-xs">{labels[f]}<input className="rounded border bg-background p-2 text-sm" disabled={busy} type={f==='port'?'number':'text'} value={String(r[f]??'')} onChange={e=>{setDirty(true);setConfig(c=>({...c,[g.key]:c[g.key].map((v,n)=>n===i?{...v,[f]:f==='port'?Number(e.target.value):e.target.value}:v)}));}}/></label>)}<Button variant="outline" disabled={busy} onClick={()=>{setDirty(true);setConfig(c=>({...c,[g.key]:c[g.key].filter((_,n)=>n!==i)}));}}>删除</Button></div>)}
 <Button variant="outline" disabled={busy||!loaded} onClick={()=>{setDirty(true);setConfig(c=>({...c,[g.key]:[...c[g.key],{...g.initial}]}));}}>添加规则</Button></section>)}
 <div className="flex gap-3"><Button disabled={busy||!loaded||!dirty} onClick={()=>void save()}>保存配置</Button><Button variant="outline" disabled={busy} onClick={()=>{if(!dirty||window.confirm('放弃未保存修改并重新加载？'))void load();}}>重新加载</Button></div>
 </div></div>;
}
