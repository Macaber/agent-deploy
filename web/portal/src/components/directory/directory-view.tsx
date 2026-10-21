import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { AnimatePresence, motion, useReducedMotion } from 'motion/react';
import {
  Activity,
  ChevronLeft,
  ChevronRight,
  ExternalLink,
  Folder,
  Inbox,
  Layers,
  Loader2,
  Pause,
  Pencil,
  Play,
  Plus,
  RefreshCw,
  Search,
  Square,
  X,
} from 'lucide-react';
import { useApp } from '@/store';
import type { DirectoryFilter, WorkspaceItem } from '@/types';
import { buttonVariants } from '@/components/ui/button';
import { Button } from '@/components/ui/button';
import { Card } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Segmented } from '@/components/ui/segmented';
import { StatusBadge } from '@/components/ui/status-badge';
import { EmptyState } from '@/components/ui/empty-state';
import { Tip } from '@/components/ui/tooltip';
import { cn, formatTime, initials, safeExternalUrl } from '@/lib/utils';
import { useMediaQuery } from '@/lib/use-media-query';

const PAGE_SIZE = 50;

const FILTERS: Array<{ value: DirectoryFilter; label: string }> = [
  { value: 'all', label: '全部' },
  { value: 'running', label: '运行中' },
  { value: 'idle', label: '待机' },
  { value: 'failed', label: '异常' },
];

function matchesFilter(item: WorkspaceItem, filter: DirectoryFilter): boolean {
  const phase = (item.phase || '').toLowerCase();
  if (filter === 'running') return phase === 'running';
  if (filter === 'idle') return phase === 'sleeping' || phase === 'stopped';
  if (filter === 'failed') return phase === 'failed';
  return true;
}

export function DirectoryView() {
  const directory = useApp((state) => state.directory);
  const fetchDirectory = useApp((state) => state.fetchDirectory);
  const setNamespace = useApp((state) => state.setDirectoryNamespace);
  const setSearch = useApp((state) => state.setDirectorySearch);
  const setFilter = useApp((state) => state.setDirectoryFilter);
  const setTab = useApp((state) => state.setTab);
  const operation = useApp((state) => state.operation);
  const auth = useApp((state) => state.auth);
  const loadWorkspace = useApp((state) => state.loadWorkspace);
  const inlineStop = useApp((state) => state.inlineStop);
  const inlineWakeup = useApp((state) => state.inlineWakeup);
  const askConfirm = useApp((state) => state.askConfirm);
  const searchRef = useRef<HTMLInputElement>(null);
  const isDesktop = useMediaQuery('(min-width: 1024px)');
  const [page, setPage] = useState(1);

  useEffect(() => {
    if (auth.checking || !auth.authenticated || auth.loginOpen) return;
    const timer = window.setInterval(() => {
      if (!document.hidden && !useApp.getState().operation) {
        void fetchDirectory(true);
      }
    }, 30_000);
    return () => window.clearInterval(timer);
  }, [auth.authenticated, auth.checking, auth.loginOpen, fetchDirectory]);

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      const target = event.target as HTMLElement | null;
      const typing =
        target?.tagName === 'INPUT' ||
        target?.tagName === 'TEXTAREA' ||
        target?.isContentEditable;
      if (event.key === '/' && !typing) {
        event.preventDefault();
        searchRef.current?.focus();
      }
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, []);

  const stats = useMemo(() => {
    let running = 0;
    let idle = 0;
    let failed = 0;
    directory.items.forEach((item) => {
      const phase = (item.phase || '').toLowerCase();
      if (phase === 'running') running += 1;
      else if (phase === 'sleeping' || phase === 'stopped') idle += 1;
      else if (phase === 'failed') failed += 1;
    });
    return { running, idle, failed, total: directory.items.length };
  }, [directory.items]);

  const filtered = useMemo(() => {
    const query = directory.search.trim().toLowerCase();
    return directory.items.filter((item) => {
      if (!matchesFilter(item, directory.filter)) return false;
      if (!query) return true;
      return `${item.userId || ''} ${item.oa || ''} ${item.name || ''} ${item.image || ''} ${
        item.namespace || ''
      }`
        .toLowerCase()
        .includes(query);
    });
  }, [directory.items, directory.filter, directory.search]);

  const confirmStop = (item: WorkspaceItem) => {
    askConfirm({
      title: '停止工作空间',
      description: `确定要停止用户 ${item.userId} 的工作空间吗？容器将被伸缩到 0，数据会保留。`,
      confirmLabel: '停止实例',
      tone: 'destructive',
      onConfirm: () => inlineStop(item.userId, item.namespace || 'bocomwork'),
    });
  };

  const initialLoading = !directory.initialized || (directory.loading && directory.items.length === 0);

  const totalPages = Math.max(1, Math.ceil(filtered.length / PAGE_SIZE));
  const safePage = Math.min(page, totalPages);
  const pageItems = filtered.slice((safePage - 1) * PAGE_SIZE, safePage * PAGE_SIZE);

  useEffect(() => {
    setPage(1);
  }, [directory.search, directory.filter, directory.namespace]);

  useEffect(() => {
    if (page > totalPages) setPage(totalPages);
  }, [page, totalPages]);
  const isEmpty =
    directory.initialized && !directory.loading && !directory.error && filtered.length === 0;

  return (
    <div className="h-full overflow-y-auto overscroll-contain">
      <div className="mx-auto flex w-full max-w-[1440px] flex-col gap-4 px-3 py-4 sm:px-5 sm:py-6">
        <div className="grid grid-cols-3 gap-2 sm:gap-3">
          <StatCard
            label="运行中"
            value={stats.running}
            hint="可立即进入 IDE"
            icon={<Play className="size-4" />}
            tone="success"
          />
          <StatCard
            label="待机 / 已停止"
            value={stats.idle}
            hint="按需唤醒，数据保留"
            icon={<Pause className="size-4" />}
            tone="warning"
          />
          <StatCard
            label="工作空间总数"
            value={stats.total}
            hint={
              stats.failed > 0
                ? `${stats.failed} 个异常`
                : directory.lastUpdated
                  ? `更新于 ${formatTime(directory.lastUpdated)}`
                  : '尚未同步'
            }
            icon={<Layers className="size-4" />}
            tone={stats.failed > 0 ? 'destructive' : 'neutral'}
          />
        </div>

        <Card className="p-3">
          <div className="flex flex-col gap-3 xl:flex-row xl:items-center xl:justify-between">
            <div className="flex flex-1 flex-col gap-2 sm:flex-row sm:flex-wrap sm:items-center">
              <div className="relative w-full sm:w-[190px]">
                <Folder className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
                <Input
                  aria-label="命名空间"
                  value={directory.namespace}
                  onChange={(event) => setNamespace(event.target.value)}
                  onKeyDown={(event) => {
                    if (event.key === 'Enter') void fetchDirectory();
                  }}
                  placeholder="命名空间，all 为全部"
                  className="pl-8 font-mono text-xs"
                  spellCheck={false}
                />
              </div>

              <div className="relative w-full sm:w-[250px]">
                <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
                <Input
                  ref={searchRef}
                  aria-label="筛选工作空间"
                  value={directory.search}
                  onChange={(event) => setSearch(event.target.value)}
                  placeholder="筛选用户 / OA / 镜像"
                  className="pl-8 pr-8"
                />
                {directory.search ? (
                  <button
                    type="button"
                    aria-label="清除筛选"
                    onClick={() => setSearch('')}
                    className="absolute right-1.5 top-1/2 grid size-6 -translate-y-1/2 place-items-center rounded text-muted-foreground hover:bg-accent hover:text-foreground"
                  >
                    <X className="size-3.5" />
                  </button>
                ) : (
                  <kbd className="pointer-events-none absolute right-2 top-1/2 hidden -translate-y-1/2 rounded border border-border bg-muted px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground sm:block">
                    /
                  </kbd>
                )}
              </div>

              <Segmented
                value={directory.filter}
                options={FILTERS}
                onChange={setFilter}
                layoutId="directory-filter"
                ariaLabel="状态筛选"
                className="w-full sm:w-auto"
              />
            </div>

            <div className="flex items-center gap-2">
              <Button
                variant="outline"
                onClick={() => void fetchDirectory()}
                disabled={directory.loading || auth.checking || !auth.authenticated}
                className="flex-1 sm:flex-none"
              >
                <RefreshCw className={cn('size-3.5', directory.loading && 'animate-spin')} />
                刷新
              </Button>
              <Button onClick={() => setTab('studio')} className="flex-1 sm:flex-none">
                <Plus className="size-4" />
                新建工作空间
              </Button>
            </div>
          </div>
        </Card>

        {directory.error && (
          <div className="flex items-center justify-between gap-3 rounded-lg border border-destructive/25 bg-destructive/8 px-3.5 py-2.5 text-xs text-destructive">
            <span className="min-w-0 truncate">{directory.error}</span>
            <Button variant="ghost" size="sm" onClick={() => void fetchDirectory()}>
              重试
            </Button>
          </div>
        )}

        <Card className="overflow-hidden">
          <div className="flex h-10 items-center justify-between gap-3 border-b border-border px-4 text-[11px] text-muted-foreground">
            <span>
              共 <span className="font-medium tabular-nums text-foreground">{filtered.length}</span>{' '}
              个实例
              {directory.search && ` · 已筛选`}
            </span>
            <span className="flex items-center gap-2">
              {directory.loading ? (
                <>
                  <Loader2 className="size-3 animate-spin" />
                  同步中
                </>
              ) : directory.lastUpdated ? (
                <>更新于 {formatTime(directory.lastUpdated)}</>
              ) : (
                '尚未同步'
              )}
            </span>
          </div>

          {isDesktop && (
          <div>
            <div className="overflow-x-auto">
              <table className="w-full min-w-[860px] border-collapse text-sm">
                <caption className="sr-only">工作空间实例列表</caption>
                <thead>
                  <tr className="border-b border-border bg-muted/30 text-left text-[11px] font-medium text-muted-foreground">
                    <th scope="col" className="px-4 py-2.5 font-medium">用户</th>
                    <th scope="col" className="px-4 py-2.5 font-medium">实例</th>
                    <th scope="col" className="px-4 py-2.5 font-medium">容器镜像</th>
                    <th scope="col" className="px-4 py-2.5 font-medium">规格</th>
                    <th scope="col" className="px-4 py-2.5 font-medium">状态</th>
                    <th scope="col" className="px-4 py-2.5 text-right font-medium">操作</th>
                  </tr>
                </thead>
                <tbody>
                  <AnimatePresence initial={false}>
                    {pageItems.map((item) => (
                      <motion.tr
                        key={`${item.namespace}/${item.name}`}
                        layout
                        initial={{ opacity: 0 }}
                        animate={{ opacity: 1 }}
                        exit={{ opacity: 0 }}
                        className="group border-b border-border/70 transition-colors last:border-0 hover:bg-muted/25"
                      >
                        <td className="px-4 py-3">
                          <div className="flex items-center gap-2.5">
                            <span className="grid size-8 shrink-0 place-items-center rounded-full border border-border bg-secondary text-[11px] font-semibold text-secondary-foreground">
                              {initials(item.userId)}
                            </span>
                            <div className="min-w-0">
                              <div className="truncate text-[13px] font-medium">{item.userId || '—'}</div>
                              <div className="mt-0.5 text-[11px] text-muted-foreground">
                                {item.oa ? (
                                  <span className="font-mono">OA {item.oa}</span>
                                ) : (
                                  '未绑定 OA'
                                )}
                              </div>
                            </div>
                          </div>
                        </td>
                        <td className="px-4 py-3">
                          <div className="truncate font-mono text-xs">{item.name || '—'}</div>
                          <div className="mt-0.5 text-[11px] text-muted-foreground">
                            {item.namespace || '—'}
                          </div>
                        </td>
                        <td className="max-w-[280px] px-4 py-3">
                          <div className="truncate font-mono text-xs text-muted-foreground" title={item.image || ''}>
                            {item.image || '—'}
                          </div>
                        </td>
                        <td className="px-4 py-3">
                          <div className="flex items-center gap-1.5 font-mono text-[11px] text-muted-foreground">
                            <span className="rounded border border-border bg-muted/40 px-1.5 py-0.5">
                              {item.cpu || '—'} CPU
                            </span>
                            <span className="rounded border border-border bg-muted/40 px-1.5 py-0.5">
                              {item.memory || '—'}
                            </span>
                          </div>
                        </td>
                        <td className="px-4 py-3">
                          <StatusBadge phase={item.phase} />
                        </td>
                        <td className="px-4 py-3">
                          <RowActions
                            item={item}
                            onStop={confirmStop}
                            onEdit={() => void loadWorkspace(item.userId, item.namespace || 'bocomwork')}
                            onWakeup={() => void inlineWakeup(item.userId, item.namespace || 'bocomwork')}
                            busy={!!operation}
                          />
                        </td>
                      </motion.tr>
                    ))}
                  </AnimatePresence>
                </tbody>
              </table>
            </div>
          </div>
          )}

          {!isDesktop && (
          <div className="divide-y divide-border">
            {pageItems.map((item) => (
              <WorkspaceCard
                key={`${item.namespace}/${item.name}`}
                item={item}
                onStop={confirmStop}
                onEdit={() => void loadWorkspace(item.userId, item.namespace || 'bocomwork')}
                onWakeup={() => void inlineWakeup(item.userId, item.namespace || 'bocomwork')}
                busy={!!operation}
              />
            ))}
          </div>
          )}

          {initialLoading && (
            <div className="space-y-2 p-3">
              {Array.from({ length: 4 }).map((_, index) => (
                <div key={index} className="flex items-center gap-3 rounded-lg border border-border/60 p-3">
                  <div className="skeleton size-8 rounded-full" />
                  <div className="flex-1 space-y-2">
                    <div className="skeleton h-3 w-32 rounded" />
                    <div className="skeleton h-2.5 w-52 rounded" />
                  </div>
                  <div className="skeleton h-5 w-16 rounded" />
                </div>
              ))}
            </div>
          )}

          {isEmpty && !initialLoading && (
            <div className="p-3">
              <EmptyState
                icon={<Inbox className="size-5" />}
                title={directory.search ? '没有匹配的实例' : '还没有工作空间'}
                description={
                  directory.search
                    ? '试试更换关键词或切换状态筛选。'
                    : directory.namespace === 'all'
                      ? '当前集群中还没有任何工作空间实例。'
                      : `命名空间 ${directory.namespace} 下还没有活跃实例。`
                }
                action={
                  <Button size="sm" onClick={() => setTab('studio')}>
                    <Plus className="size-3.5" />
                    创建第一个工作空间
                  </Button>
                }
              />
            </div>
          )}

          {totalPages > 1 && (
            <div className="flex items-center justify-between gap-3 border-t border-border px-4 py-2 text-[11px] text-muted-foreground">
              <span>
                第 <span className="font-medium tabular-nums text-foreground">{safePage}</span> /{' '}
                {totalPages} 页
              </span>
              <div className="flex items-center gap-1">
                <Button
                  variant="outline"
                  size="iconSm"
                  aria-label="上一页"
                  disabled={safePage <= 1}
                  onClick={() => setPage((value) => Math.max(1, value - 1))}
                >
                  <ChevronLeft className="size-3.5" />
                </Button>
                <Button
                  variant="outline"
                  size="iconSm"
                  aria-label="下一页"
                  disabled={safePage >= totalPages}
                  onClick={() => setPage((value) => Math.min(totalPages, value + 1))}
                >
                  <ChevronRight className="size-3.5" />
                </Button>
              </div>
            </div>
          )}
        </Card>

        <div className="flex items-center justify-between px-1 pb-2 text-[11px] text-muted-foreground">
          <span className="inline-flex items-center gap-1.5">
            <Activity className="size-3" />
            列表每 30 秒自动同步
          </span>
          <span>当前命名空间：{directory.namespace || 'bocomwork'}</span>
        </div>
      </div>
    </div>
  );
}

function StatCard({
  label,
  value,
  hint,
  icon,
  tone,
}: {
  label: string;
  value: number;
  hint: string;
  icon: ReactNode;
  tone: 'success' | 'warning' | 'destructive' | 'neutral';
}) {
  const toneClass = {
    success: 'border-success/20 bg-success/8 text-success',
    warning: 'border-warning/20 bg-warning/8 text-warning',
    destructive: 'border-destructive/20 bg-destructive/8 text-destructive',
    neutral: 'border-border bg-muted/40 text-muted-foreground',
  }[tone];

  return (
    <Card className="flex items-start justify-between gap-2 p-2.5 sm:p-4">
      <div className="min-w-0">
        <p className="truncate text-[10px] font-medium text-muted-foreground sm:text-xs">{label}</p>
        <p className="mt-1 text-xl font-semibold leading-none tracking-tight tabular-nums sm:mt-1.5 sm:text-2xl">
          {value}
        </p>
        <p className="mt-1.5 truncate text-[10px] text-muted-foreground sm:mt-2 sm:text-[11px]" title={hint}>
          {hint}
        </p>
      </div>
      <span className={cn('hidden size-9 shrink-0 place-items-center rounded-lg border sm:grid', toneClass)}>
        {icon}
      </span>
    </Card>
  );
}

function RowActions({
  item,
  onStop,
  onEdit,
  onWakeup,
  busy,
}: {
  item: WorkspaceItem;
  onStop: (item: WorkspaceItem) => void;
  onEdit: () => void;
  onWakeup: () => void;
  busy: boolean;
}) {
  const phase = (item.phase || '').toLowerCase();
  const url = safeExternalUrl(item.url);
  return (
    <div className="flex items-center justify-end gap-1">
      {phase === 'running' && url && (
        <a
          href={url}
          target="_blank"
          rel="noreferrer noopener"
          className={cn(buttonVariants({ variant: 'outline', size: 'sm' }))}
        >
          <ExternalLink className="size-3.5" />
          进入
        </a>
      )}
      {(phase === 'sleeping' || phase === 'stopped') && (
        <Button variant="outline" size="sm" onClick={onWakeup} disabled={busy}>
          <Play className="size-3.5" />
          唤醒
        </Button>
      )}
      {(phase === 'running' || phase === 'starting') && (
        <Tip label="停止实例">
          <Button variant="ghost" size="iconSm" onClick={() => onStop(item)} disabled={busy} aria-label={`停止 ${item.userId}`}>
            <Square className="size-3.5 text-destructive" />
          </Button>
        </Tip>
      )}
      <Tip label="载入到编排设计器">
        <Button variant="ghost" size="iconSm" onClick={onEdit} disabled={busy} aria-label={`编辑 ${item.userId}`}>
          <Pencil className="size-3.5" />
        </Button>
      </Tip>
    </div>
  );
}

function WorkspaceCard({
  item,
  onStop,
  onEdit,
  onWakeup,
  busy,
}: {
  item: WorkspaceItem;
  onStop: (item: WorkspaceItem) => void;
  onEdit: () => void;
  onWakeup: () => void;
  busy: boolean;
}) {
  const phase = (item.phase || '').toLowerCase();
  const url = safeExternalUrl(item.url);
  return (
    <div className="space-y-3 p-3.5">
      <div className="flex items-start justify-between gap-3">
        <div className="flex min-w-0 items-center gap-2.5">
          <span className="grid size-8 shrink-0 place-items-center rounded-full border border-border bg-secondary text-[11px] font-semibold">
            {initials(item.userId)}
          </span>
          <div className="min-w-0">
            <div className="truncate text-[13px] font-medium">{item.userId || '—'}</div>
            <div className="truncate text-[11px] text-muted-foreground">
              {item.oa ? `OA ${item.oa}` : item.namespace || '—'}
            </div>
          </div>
        </div>
        <StatusBadge phase={item.phase} />
      </div>

      <div className="rounded-lg border border-border/70 bg-muted/20 px-2.5 py-2">
        <div className="truncate font-mono text-[11px] text-muted-foreground" title={item.image || ''}>
          {item.image || '—'}
        </div>
        <div className="mt-1 flex items-center gap-2 font-mono text-[10px] text-muted-foreground">
          <span>{item.name}</span>
          <span className="text-border">·</span>
          <span>{item.cpu || '—'} CPU</span>
          <span>{item.memory || '—'}</span>
        </div>
      </div>

      <div className="flex items-center gap-2">
        {phase === 'running' && url && (
          <a
            href={url}
            target="_blank"
            rel="noreferrer noopener"
            className={cn(buttonVariants({ variant: 'outline', size: 'sm' }), 'flex-1')}
          >
            <ExternalLink className="size-3.5" />
            进入 IDE
          </a>
        )}
        {(phase === 'sleeping' || phase === 'stopped') && (
          <Button variant="outline" size="sm" onClick={onWakeup} disabled={busy} className="flex-1">
            <Play className="size-3.5" />
            唤醒
          </Button>
        )}
        {(phase === 'running' || phase === 'starting') && (
          <Button variant="destructive" size="sm" onClick={() => onStop(item)} disabled={busy}>
            <Square className="size-3.5" />
            停止
          </Button>
        )}
        <Button variant="secondary" size="sm" onClick={onEdit} disabled={busy} className="flex-1">
          <Pencil className="size-3.5" />
          编辑配置
        </Button>
      </div>
    </div>
  );
}
