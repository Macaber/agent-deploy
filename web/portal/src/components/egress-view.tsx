import { useEffect, useState } from 'react';
import {
  ShieldCheck,
  Globe,
  Network,
  ArrowUpRight,
  Plus,
  Trash2,
  Copy,
  ClipboardPaste,
  RotateCcw,
  Check,
  CircleAlert,
  Loader2,
  RefreshCw,
  Server,
  Layers,
} from 'lucide-react';
import { apiUrl } from '@/lib/api';
import { useApp } from '@/store';
import { parseBulk, formatBulk, type BulkGroup } from '@/lib/egress-bulk';
import { Button } from '@/components/ui/button';
import { Card, CardHeader, CardTitle, CardDescription, CardContent } from '@/components/ui/card';
import { Input, Textarea } from '@/components/ui/input';
import { Badge } from '@/components/ui/badge';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import {
  SpotlightCard,
  CollapsibleMotion,
  BorderTrail,
} from '@/components/ui/motion';
import { cn } from '@/lib/utils';

type Replica = {
  pod: string;
  state: string;
  loadedRevision: string;
  observedAt: string;
  error: string;
};

type SyncStatus = {
  pendingWorkspaces?: string[];
  revision: string;
  proxyState: string;
  desiredReplicas: number;
  replicas: Replica[];
  policyState: string;
  migrationState: string;
};

const STATE_LABEL: Record<string, string> = {
  loaded: '已加载',
  syncing: '同步中',
  failed: '同步失败',
  unverified: '尚未验证',
  not_required: '此模式无需代理',
  pending: '等待 Pod 切换完成',
  observed: '已观测到 Pod 模式一致（网络策略尚未验证）',
};

type Row = Record<string, string | number | Record<string, string>>;
type Mode = 'legacy' | 'allowlist' | 'unrestricted';

const MODE_LABELS: Record<Mode, string> = {
  legacy: '恢复原有默认策略',
  allowlist: '启用白名单',
  unrestricted: '全部出站开放',
};

const MODE_DESCRIPTIONS: Record<Mode, string> = {
  legacy: '沿用原有网络策略，仅放行平台默认服务',
  allowlist: '按配置的 IP/CIDR、域名及基础服务放行',
  unrestricted: '开放所有 Workspace 出站访问（直连）',
};

const MODE_ICONS: Record<Mode, typeof ShieldCheck> = {
  legacy: ShieldCheck,
  allowlist: Network,
  unrestricted: Globe,
};

type Config = Record<'ips' | 'domains' | 'services', Row[]> & { mode?: Mode };

const GROUPS: Array<{
  key: BulkGroup;
  title: string;
  description: string;
  fields: string[];
  initial: Row;
}> = [
  {
    key: 'ips',
    title: 'IP / CIDR 规则',
    description: '允许直连或通过代理放行的 IPv4 地址段及端口',
    fields: ['cidr', 'protocol', 'port'],
    initial: { cidr: '', protocol: 'TCP', port: 443 },
  },
  {
    key: 'domains',
    title: '域名代理规则',
    description: '允许 HTTP/HTTPS 代理访问的外部域名',
    fields: ['host', 'port'],
    initial: { host: '', port: 443 },
  },
  {
    key: 'services',
    title: '必要基础服务规则',
    description: 'Kubernetes 集群内基础服务放行（如 DNS、监控等）',
    fields: ['name', 'namespace', 'podSelector', 'protocol', 'port'],
    initial: { name: '', namespace: '', podSelector: '', protocol: 'UDP', port: 53 },
  },
];

const LABELS: Record<string, string> = {
  cidr: 'IP / CIDR',
  protocol: '协议',
  port: '端口',
  host: '完整域名',
  name: '服务名称 / 用途',
  namespace: '命名空间',
  podSelector: 'Pod 标签 (key=value，逗号分隔)',
};

async function request(init?: RequestInit) {
  const r = await fetch(apiUrl('api/egress/config'), {
    credentials: 'same-origin',
    ...init,
    headers: { 'Content-Type': 'application/json' },
  });
  if (r.status === 401) window.dispatchEvent(new CustomEvent('bw:unauthorized'));
  if (!r.ok) {
    throw new Error(
      r.status === 409
        ? '其他管理员已修改配置，请重新加载后合并。'
        : await r.text(),
    );
  }
  return r.json();
}

export function EgressView() {
  const authenticated = useApp((s) => s.auth.authenticated);
  const askConfirm = useApp((s) => s.askConfirm);
  const pushToast = useApp((s) => s.pushToast);

  const [bulk, setBulk] = useState<Record<BulkGroup, string>>({ ips: '', domains: '', services: '' });
  const [openBulk, setOpenBulk] = useState<Record<BulkGroup, boolean>>({
    ips: false,
    domains: false,
    services: false,
  });
  const [bulkMessage, setBulkMessage] = useState('');
  const [syncStatus, setSyncStatus] = useState<SyncStatus | null>(null);
  const [statusError, setStatusError] = useState('');

  // Config state
  const [config, setConfig] = useState<Config>({ ips: [], domains: [], services: [] });
  const [version, setVersion] = useState('');
  const [busy, setBusy] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [dirty, setDirty] = useState(false);
  const [message, setMessage] = useState('尚未读取');
  const [error, setError] = useState('');

  // Poll sync status
  useEffect(() => {
    if (!authenticated) {
      setSyncStatus(null);
      return;
    }
    let stopped = false;
    let timer: ReturnType<typeof setTimeout>;
    const controller = new AbortController();

    async function refresh() {
      try {
        const response = await fetch(apiUrl('api/egress/status'), {
          credentials: 'same-origin',
          signal: controller.signal,
        });
        if (response.status === 401) window.dispatchEvent(new CustomEvent('bw:unauthorized'));
        if (!response.ok) throw new Error(await response.text());
        const status = (await response.json()) as SyncStatus;
        if (!stopped) {
          setSyncStatus(status);
          setStatusError('');
        }
      } catch (e) {
        if (!stopped) {
          setSyncStatus(null);
          setStatusError(String(e));
        }
      } finally {
        if (!stopped) timer = setTimeout(() => void refresh(), 5000);
      }
    }

    void refresh();
    return () => {
      stopped = true;
      clearTimeout(timer);
      controller.abort();
    };
  }, [authenticated]);

  async function load() {
    setBusy(true);
    setError('');
    try {
      const d = await request();
      const c = d.config as Config;
      setConfig({
        mode: c.mode || (d.state === 'unconfigured' ? 'legacy' : 'allowlist'),
        ips: c.ips || [],
        domains: c.domains || [],
        services: (c.services || []).map((r) => ({
          ...r,
          podSelector: Object.entries(r.podSelector || {})
            .map(([k, v]) => `${k}=${v}`)
            .join(','),
        })),
      });
      setVersion(d.resourceVersion);
      setDirty(d.state === 'unconfigured');
      setLoaded(true);
      setMessage(
        (d.state === 'unconfigured' ? '尚未配置' : '已保存；尚未确认执行组件加载状态') +
          ((d.warnings || []).length ? '；检查项：' + d.warnings.join('；') : ''),
      );
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }

  useEffect(() => {
    if (authenticated) void load();
  }, [authenticated]);

  async function performSave(targetMode?: Mode) {
    const isModeChange = targetMode !== undefined && targetMode !== (config.mode || 'allowlist');
    setBusy(true);
    setError('');
    try {
      const services = config.services.map((r) => {
        const selector: Record<string, string> = {};
        for (const pair of String(r.podSelector).split(',')) {
          const at = pair.indexOf('=');
          if (at < 1 || at === pair.length - 1) throw new Error('Pod 标签须为 key=value 格式');
          const key = pair.slice(0, at).trim();
          if (Object.hasOwn(selector, key)) throw new Error('Pod 标签键重复');
          selector[key] = pair.slice(at + 1).trim();
        }
        return { ...r, podSelector: selector };
      });

      await request({
        method: 'PUT',
        body: JSON.stringify({
          config: { ...config, services, mode: targetMode || config.mode },
          resourceVersion: version,
          confirmModeChange: isModeChange,
        }),
      });

      pushToast(
        'success',
        isModeChange ? '出站模式已切换' : '出站规则已保存',
        isModeChange
          ? `已切换至“${MODE_LABELS[targetMode!]}”，相关 Workspace Pod 将按需重建。`
          : '出站网络策略与规则已成功更新并下发。',
      );
      await load();
    } catch (e) {
      setError(String(e));
      pushToast('error', '保存失败', String(e));
    } finally {
      setBusy(false);
    }
  }

  function handleSaveMode(targetMode: Mode) {
    if (targetMode === (config.mode || 'allowlist')) return;

    askConfirm({
      title: `确认切换为“${MODE_LABELS[targetMode]}”`,
      description: `切换出站模式将按序重建所有受影响的运行中 Workspace Pod，期间工作空间连接可能会短暂中断。已有放行规则将继续保留。确认继续吗？`,
      confirmLabel: '确认切换模式',
      tone: 'default',
      onConfirm: () => performSave(targetMode),
    });
  }

  function handleReload() {
    if (dirty) {
      askConfirm({
        title: '放弃未保存修改',
        description: '当前表单中有未保存的配置更改，重新加载将丢失这些更改。确认重新加载？',
        confirmLabel: '放弃修改并重载',
        tone: 'destructive',
        onConfirm: () => load(),
      });
    } else {
      void load();
    }
  }

  function appendBulk(group: BulkGroup) {
    try {
      const rows = parseBulk(group, bulk[group]);
      if (config.ips.length + config.domains.length + config.services.length + rows.length > 1000) {
        throw new Error('三类规则合计最多 1000 条');
      }
      const seen = new Set(config[group].map((r) => formatBulk(group, [r])));
      for (const row of rows) {
        const key = formatBulk(group, [row]);
        if (seen.has(key)) throw new Error('存在重复规则，请检查输入和当前表单');
        seen.add(key);
      }
      setConfig((c) => ({ ...c, [group]: [...c[group], ...rows] }));
      setDirty(true);
      setError('');
      setBulk((b) => ({ ...b, [group]: '' }));
      setBulkMessage(`已成功追加 ${rows.length} 条规则，请检查并保存生效。`);
      pushToast(
        'info',
        '批量规则已追加',
        `已成功向 ${GROUPS.find((g) => g.key === group)?.title} 追加 ${rows.length} 条规则。`,
      );
    } catch (e) {
      setError(String(e));
    }
  }

  async function pasteBulk(group: BulkGroup) {
    try {
      const text = await navigator.clipboard.readText();
      setBulk((b) => ({ ...b, [group]: text }));
      setBulkMessage('已粘贴剪贴板内容，请点击“追加到表单”。');
    } catch {
      setBulkMessage('浏览器未允许读取剪贴板，请直接在输入框内粘贴。');
    }
  }

  async function copyBulk(group: BulkGroup) {
    const text = formatBulk(group, config[group]);
    setBulk((b) => ({ ...b, [group]: text }));
    try {
      await navigator.clipboard.writeText(text);
      setBulkMessage('已复制当前表单规则到剪贴板。');
      pushToast('success', '已复制规则', '规则文本已成功复制到剪贴板。');
    } catch {
      setBulkMessage('规则已填入文本框，请手动全选复制。');
    }
  }

  return (
    <div className="h-full overflow-y-auto overscroll-contain">
      <div className="mx-auto flex w-full max-w-[1440px] flex-col gap-5 px-3 py-4 sm:px-6 sm:py-6">
        {/* Header */}
        <header className="console-page-heading">
          <div>
            <p className="console-eyebrow">网络与安全</p>
            <h1>全局出站控制</h1>
            <p>统一管理所有 Workspace 的出站工作模式与访问规则，保障网络隔离与安全合规。</p>
          </div>
          <div className="flex items-center gap-2">
            <span className="inline-flex items-center gap-1.5 rounded-full border border-border bg-card px-3 py-1 text-xs font-medium text-muted-foreground shadow-2xs">
              <Globe className="size-3.5 text-primary" strokeWidth={1.5} />
              全局生效
            </span>
          </div>
        </header>

        {/* Global Notifications */}
        {error && (
          <div className="flex items-center gap-2.5 rounded-lg border border-destructive/25 bg-destructive/10 p-3.5 text-xs text-destructive">
            <CircleAlert className="size-4 shrink-0" strokeWidth={1.5} />
            <span className="flex-1 break-words">{error}</span>
          </div>
        )}

        {bulkMessage && (
          <div className="flex items-center gap-2.5 rounded-lg border border-border bg-muted/60 p-3 text-xs text-foreground">
            <Check className="size-4 shrink-0 text-success" strokeWidth={1.5} />
            <span className="flex-1">{bulkMessage}</span>
          </div>
        )}

        {/* Mode Selector Card */}
        <Card>
          <CardHeader>
            <div className="flex flex-wrap items-center justify-between gap-2">
              <div>
                <CardTitle className="text-base font-semibold">出站工作模式</CardTitle>
                <CardDescription className="mt-1">
                  当前生效模式：<strong className="text-foreground">{MODE_LABELS[config.mode || 'allowlist']}</strong>。切换模式将按序重建运行中的 Workspace Pod；同模式下更新放行规则不会重建 Pod。
                </CardDescription>
              </div>
              {dirty && (
                <Badge variant="warning" className="gap-1">
                  有未保存修改
                </Badge>
              )}
            </div>
          </CardHeader>
          <CardContent>
            <div className="grid grid-cols-1 gap-3 md:grid-cols-3">
              {(Object.keys(MODE_LABELS) as Mode[]).map((mode) => {
                const isSelected = mode === (config.mode || 'allowlist');
                const Icon = MODE_ICONS[mode];
                return (
                  <SpotlightCard
                    key={mode}
                    className={cn(
                      'p-0 transition-all',
                      isSelected
                        ? 'border-primary bg-primary/5 shadow-2xs ring-1 ring-primary'
                        : 'border-border bg-card hover:border-ring/60 hover:bg-muted/30',
                      (busy || !loaded || dirty) && !isSelected && 'cursor-not-allowed opacity-60',
                    )}
                  >
                    <button
                      type="button"
                      disabled={busy || !loaded || dirty || isSelected}
                      onClick={() => handleSaveMode(mode)}
                      className="flex size-full flex-col justify-between gap-3 p-4 text-left outline-none focus-visible:ring-2 focus-visible:ring-ring/40"
                    >
                      <div className="flex items-start justify-between gap-2">
                        <div className="flex items-center gap-2.5">
                          <span
                            className={cn(
                              'grid size-9 place-items-center rounded-lg border',
                              isSelected
                                ? 'border-primary/30 bg-primary/10 text-primary'
                                : 'border-border bg-muted text-muted-foreground',
                            )}
                          >
                            <Icon className="size-4" strokeWidth={1.5} />
                          </span>
                          <div>
                            <p className="text-sm font-semibold text-foreground">{MODE_LABELS[mode]}</p>
                            {isSelected && (
                              <span className="inline-flex items-center gap-1 text-[11px] font-medium text-primary">
                                <Check className="size-3" strokeWidth={1.5} />
                                当前使用中
                              </span>
                            )}
                          </div>
                        </div>
                        {!isSelected && (
                          <ArrowUpRight className="size-4 text-muted-foreground" strokeWidth={1.5} />
                        )}
                      </div>
                      <p className="text-xs leading-relaxed text-muted-foreground">
                        {MODE_DESCRIPTIONS[mode]}
                      </p>
                    </button>
                  </SpotlightCard>
                );
              })}
            </div>
            {dirty && (
              <p className="mt-3 text-xs text-warning">
                提示：当前有未保存的规则修改，请先保存或放弃修改再切换出站模式。
              </p>
            )}
          </CardContent>
        </Card>

        {/* Sync & Agent Status Card */}
        <Card>
          <CardHeader>
            <div className="flex items-center justify-between gap-2">
              <div>
                <CardTitle className="text-base font-semibold">网络代理与同步状态</CardTitle>
                <CardDescription className="mt-1">
                  出站代理副本健康度、网络策略（NetworkPolicy）应用状态以及 Workspace 迁移进度。
                </CardDescription>
              </div>
              <Badge variant="outline" className="gap-1.5 font-mono text-xs">
                <Server className="size-3 text-muted-foreground" strokeWidth={1.5} />
                {syncStatus ? `期望副本: ${syncStatus.desiredReplicas}` : '等待同步'}
              </Badge>
            </div>
          </CardHeader>
          <CardContent className="space-y-4">
            {statusError ? (
              <div className="flex items-center gap-2 rounded-lg border border-destructive/20 bg-destructive/5 p-3 text-xs text-destructive">
                <CircleAlert className="size-4 shrink-0" strokeWidth={1.5} />
                <span>无法确认状态：{statusError}</span>
              </div>
            ) : syncStatus ? (
              <>
                <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
                  <div className="rounded-lg border border-border bg-muted/30 p-3">
                    <p className="text-[11px] text-muted-foreground">代理组件状态</p>
                    <p className="mt-1 text-sm font-semibold text-foreground">
                      {STATE_LABEL[syncStatus.proxyState] || syncStatus.proxyState}
                    </p>
                  </div>
                  <div className="rounded-lg border border-border bg-muted/30 p-3">
                    <p className="text-[11px] text-muted-foreground">网络策略状态</p>
                    <p className="mt-1 text-sm font-semibold text-foreground">
                      {STATE_LABEL[syncStatus.policyState] || syncStatus.policyState}
                    </p>
                  </div>
                  <div className="rounded-lg border border-border bg-muted/30 p-3">
                    <p className="text-[11px] text-muted-foreground">Workspace 迁移</p>
                    <p className="mt-1 text-sm font-semibold text-foreground">
                      {STATE_LABEL[syncStatus.migrationState] || syncStatus.migrationState}
                    </p>
                  </div>
                  <div className="rounded-lg border border-border bg-muted/30 p-3">
                    <p className="text-[11px] text-muted-foreground">配置版本 (Revision)</p>
                    <p className="mt-1 truncate font-mono text-xs font-semibold text-foreground" title={syncStatus.revision}>
                      {syncStatus.revision || '—'}
                    </p>
                  </div>
                </div>

                {syncStatus.replicas && syncStatus.replicas.length > 0 && (
                  <div className="overflow-hidden rounded-lg border border-border">
                    <Table>
                      <TableHeader>
                        <TableRow>
                          <TableHead>Pod 实例</TableHead>
                          <TableHead>运行状态</TableHead>
                          <TableHead>已加载版本</TableHead>
                          <TableHead>最近汇报时间</TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {syncStatus.replicas.map((replica) => (
                          <TableRow key={replica.pod}>
                            <TableCell className="font-mono text-xs">{replica.pod}</TableCell>
                            <TableCell>
                              <span className="inline-flex items-center gap-1.5 text-xs">
                                <span
                                  className={cn(
                                    'size-1.5 rounded-full',
                                    replica.state === 'loaded' ? 'bg-success' : 'bg-warning',
                                  )}
                                />
                                {STATE_LABEL[replica.state] || replica.state}
                              </span>
                              {replica.error && (
                                <p className="mt-0.5 text-[11px] text-destructive">{replica.error}</p>
                              )}
                            </TableCell>
                            <TableCell className="font-mono text-xs text-muted-foreground">
                              {replica.loadedRevision?.slice(0, 12) || '—'}
                            </TableCell>
                            <TableCell className="text-xs text-muted-foreground">
                              {replica.observedAt || '—'}
                            </TableCell>
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                  </div>
                )}

                {syncStatus.pendingWorkspaces && syncStatus.pendingWorkspaces.length > 0 && (
                  <div className="rounded-lg border border-warning/25 bg-warning/8 p-3 text-xs text-warning">
                    <p className="font-medium">待完成迁移的 Workspace：</p>
                    <p className="mt-1 font-mono">{syncStatus.pendingWorkspaces.join('、')}</p>
                  </div>
                )}
              </>
            ) : (
              <div className="flex items-center gap-2 py-3 text-xs text-muted-foreground">
                <Loader2 className="size-4 animate-spin" strokeWidth={1.5} />
                正在获取代理状态…
              </div>
            )}
          </CardContent>
        </Card>

        {/* Rule Groups Cards */}
        {GROUPS.map((g) => {
          const list = config[g.key] || [];
          const isBulkOpen = openBulk[g.key];
          return (
            <Card key={g.key}>
              <CardHeader>
                <div className="flex flex-wrap items-center justify-between gap-3">
                  <div>
                    <div className="flex items-center gap-2">
                      <CardTitle className="text-base font-semibold">{g.title}</CardTitle>
                      <Badge variant="outline" className="font-mono text-xs tabular-nums">
                        {loaded ? `${list.length} 条` : '加载中'}
                      </Badge>
                    </div>
                    <CardDescription className="mt-1">{g.description}</CardDescription>
                  </div>
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => setOpenBulk((s) => ({ ...s, [g.key]: !s[g.key] }))}
                  >
                    <ClipboardPaste className="size-3.5" strokeWidth={1.5} />
                    {isBulkOpen ? '收起批量导入' : '批量导入 / 导出'}
                  </Button>
                </div>
              </CardHeader>
              <CardContent className="space-y-4">
                {/* Collapsible Bulk Area */}
                <CollapsibleMotion open={isBulkOpen}>
                  <div className="mb-4 space-y-3 rounded-lg border border-border bg-muted/30 p-4">
                    <div className="space-y-1">
                      <p className="text-xs font-semibold text-foreground">批量规则输入（支持从 Excel 复制）</p>
                      <p className="text-[11px] leading-relaxed text-muted-foreground">
                        每行一条，按以下顺序用制表符（Tab）或空格分隔：
                        <span className="font-mono font-medium text-foreground">
                          {' ' + g.fields.map((f) => LABELS[f]).join(' → ')}
                        </span>
                        。追加后需点击底部“保存配置”正式提交。
                      </p>
                    </div>
                    <Textarea
                      aria-label={`${g.title} 批量输入`}
                      className="min-h-28 font-mono text-xs"
                      disabled={busy || !loaded}
                      value={bulk[g.key]}
                      onChange={(e) => setBulk((b) => ({ ...b, [g.key]: e.target.value }))}
                      placeholder={
                        g.key === 'ips'
                          ? '192.0.2.1/32 TCP 443\n10.0.0.0/8 TCP 80'
                          : g.key === 'domains'
                            ? 'api.github.com 443\nregistry.npmjs.org 443'
                            : 'CoreDNS kube-system k8s-app=kube-dns UDP 53'
                      }
                    />
                    <div className="flex flex-wrap items-center gap-2">
                      <Button
                        variant="secondary"
                        size="sm"
                        disabled={busy || !loaded}
                        onClick={() => void pasteBulk(g.key)}
                      >
                        <ClipboardPaste className="size-3.5" strokeWidth={1.5} />
                        粘贴剪贴板
                      </Button>
                      <Button
                        size="sm"
                        disabled={busy || !loaded || !bulk[g.key].trim()}
                        onClick={() => appendBulk(g.key)}
                      >
                        <Plus className="size-3.5" strokeWidth={1.5} />
                        追加到表单
                      </Button>
                      <Button
                        variant="outline"
                        size="sm"
                        disabled={busy || !loaded || list.length === 0}
                        onClick={() => void copyBulk(g.key)}
                      >
                        <Copy className="size-3.5" strokeWidth={1.5} />
                        复制当前规则
                      </Button>
                    </div>
                  </div>
                </CollapsibleMotion>

                {/* Rules Table / Row List */}
                {loaded && list.length === 0 ? (
                  <div className="rounded-lg border border-dashed border-border p-6 text-center text-xs text-muted-foreground">
                    暂无规则。可点击下方“添加规则”或使用上方“批量导入”。
                  </div>
                ) : (
                  <div className="space-y-2">
                    {list.map((r, i) => (
                      <div
                        key={i}
                        className="flex flex-col gap-2 rounded-lg border border-border bg-card p-3 shadow-2xs sm:flex-row sm:items-center"
                      >
                        <div className="grid flex-1 grid-cols-1 gap-2 sm:grid-cols-2 md:grid-cols-3 lg:grid-cols-5">
                          {g.fields.map((f) => (
                            <div key={f} className="flex min-w-0 flex-col gap-1">
                              <label className="truncate text-[10px] font-medium text-muted-foreground">
                                {LABELS[f]}
                              </label>
                              <Input
                                disabled={busy}
                                type={f === 'port' ? 'number' : 'text'}
                                value={String(r[f] ?? '')}
                                onChange={(e) => {
                                  setDirty(true);
                                  setConfig((c) => ({
                                    ...c,
                                    [g.key]: c[g.key].map((v, n) =>
                                      n === i
                                        ? {
                                            ...v,
                                            [f]: f === 'port' ? Number(e.target.value) : e.target.value,
                                          }
                                        : v,
                                    ),
                                  }));
                                }}
                                className="h-8 font-mono text-xs"
                                placeholder={LABELS[f]}
                              />
                            </div>
                          ))}
                        </div>
                        <Button
                          variant="ghost"
                          size="iconSm"
                          disabled={busy}
                          onClick={() => {
                            setDirty(true);
                            setConfig((c) => ({
                              ...c,
                              [g.key]: c[g.key].filter((_, n) => n !== i),
                            }));
                          }}
                          className="self-end text-muted-foreground hover:bg-destructive/10 hover:text-destructive sm:self-center"
                          title="删除此规则"
                        >
                          <Trash2 className="size-4" strokeWidth={1.5} />
                        </Button>
                      </div>
                    ))}
                  </div>
                )}

                <Button
                  variant="outline"
                  size="sm"
                  disabled={busy || !loaded}
                  onClick={() => {
                    setDirty(true);
                    setConfig((c) => ({
                      ...c,
                      [g.key]: [...c[g.key], { ...g.initial }],
                    }));
                  }}
                  className="mt-2"
                >
                  <Plus className="size-3.5" strokeWidth={1.5} />
                  添加单条规则
                </Button>
              </CardContent>
            </Card>
          );
        })}

        {/* Action Bar */}
        <div className="sticky bottom-3 z-30 flex items-center justify-between gap-3 overflow-hidden rounded-xl border border-border bg-card/90 p-4 shadow-lg backdrop-blur-md">
          {dirty && <BorderTrail size={20} duration={4} />}
          <div className="flex items-center gap-2">
            <span className="text-xs text-muted-foreground">
              {dirty ? (
                <span className="font-medium text-warning">有未保存修改</span>
              ) : (
                <span>配置已是最新</span>
              )}
              {message && ` · ${message}`}
            </span>
          </div>
          <div className="flex items-center gap-2">
            <Button
              variant="outline"
              disabled={busy}
              onClick={handleReload}
            >
              <RotateCcw className="size-3.5" strokeWidth={1.5} />
              重新加载
            </Button>
            <Button
              disabled={busy || !loaded || !dirty}
              onClick={() => void performSave()}
            >
              {busy ? <Loader2 className="size-3.5 animate-spin" strokeWidth={1.5} /> : <Check className="size-3.5" strokeWidth={1.5} />}
              保存配置
            </Button>
          </div>
        </div>
      </div>
    </div>
  );
}
