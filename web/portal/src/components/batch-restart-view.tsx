import { useEffect, useState, type ReactNode } from 'react';
import {
  Activity,
  CircleAlert,
  CircleCheck,
  FileJson,
  Play,
  Settings2,
  Square,
  Timer,
} from 'lucide-react';
import { apiUrl } from '@/lib/api';
import { useApp } from '@/store';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card } from '@/components/ui/card';
import { Field } from '@/components/ui/field';
import { Input } from '@/components/ui/input';
import { cn } from '@/lib/utils';
import type { BatchStatus } from '@/types';

const TEMPLATE_JSON = `{
  "image": "docker.io/library/bocomwork:v1.0.2",
  "port": 3000,
  "cpu": "0.5",
  "memory": "1Gi",
  "healthPath": "/health",
  "command": ["bocomwork-entrypoint"],
  "args": [],
  "postStartScript": "",
  "env": [
    { "name": "OPENCODE_RESOURCE_ATTRIBUTES", "value": "service.name=opencode,workspace.owner=$(WORKSPACE_OWNER),workspace.name=$(WORKSPACE_NAME)" },
    { "name": "AIONUI_DATA_DIR", "value": "/root/.bocomwork" },
    { "name": "AIONUI_HOST", "value": "0.0.0.0" },
    { "name": "WEBUI_PORT", "value": "3000" },
    { "name": "AIONUI_ALLOW_REMOTE", "value": "1" },
    { "name": "SKILLHUB_PLATFORM", "value": "BOCOMCODE" },
    { "name": "OPENCODE_CONFIG", "value": "/root/config/bocomcode.json" },
    { "name": "TZ", "value": "Asia/Shanghai" }
  ],
  "volumeMounts": [
    { "mountPath": "/root/.bocomwork", "subPath": "bocomwork" },
    { "mountPath": "/root/.local/share/opencode", "subPath": "data" },
    { "mountPath": "/root/.config/opencode", "subPath": "data" }
  ],
  "sharedVolumeMounts": [
    { "pvcName": "bocomwork-local-share", "mountPath": "/opt/bocom-defaults/skills/", "subPath": "skills", "readOnly": false },
    { "pvcName": "bocomwork-local-share", "mountPath": "/opt/bocom-defaults/dist/", "subPath": "dist", "readOnly": false },
    { "pvcName": "bocomwork-local-share", "mountPath": "/opt/bocom-defaults/opencodedir/", "subPath": "opencodedir", "readOnly": false }
  ]
}`;

const STATE_LABELS: Record<string, string> = {
  none: '空闲',
  waiting: '等待开始',
  running: '运行中',
  stopping: '停止中',
  completed: '已完成',
  stopped: '已停止',
};

const COUNTERS: Array<{ key: keyof BatchStatus; label: string; tone: string }> = [
  { key: 'total', label: '总目标', tone: 'text-foreground' },
  { key: 'stopped', label: '已完成', tone: 'text-success' },
  { key: 'inFlight', label: '进行中', tone: 'text-info' },
  { key: 'pending', label: '待启动', tone: 'text-muted-foreground' },
  { key: 'failed', label: '失败', tone: 'text-destructive' },
  { key: 'skipped', label: '已跳过', tone: 'text-muted-foreground' },
];

function localDatetimeMin() {
  const now = new Date();
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())}T${pad(now.getHours())}:${pad(now.getMinutes())}`;
}

function Section({
  icon,
  title,
  badge,
  action,
  children,
}: {
  icon: ReactNode;
  title: string;
  badge?: ReactNode;
  action?: ReactNode;
  children: ReactNode;
}) {
  return (
    <Card className="studio-section overflow-hidden p-0" data-section={title}>
      <div className="flex flex-wrap items-center justify-between gap-2 px-0 py-2.5">
        <div className="flex min-w-0 items-center gap-2.5">
          <span className="studio-section-icon grid size-7 shrink-0 place-items-center rounded-md text-muted-foreground">
            {icon}
          </span>
          <div className="min-w-0">
            <div className="flex items-center gap-2">
              <h2 className="truncate text-sm font-semibold tracking-tight">{title}</h2>
              {badge}
            </div>
          </div>
        </div>
        {action && <div className="ml-auto">{action}</div>}
      </div>
      <div className="pb-2">{children}</div>
    </Card>
  );
}

export function BatchRestartView() {
  const authenticated = useApp((s) => s.auth.authenticated);
  const askConfirm = useApp((s) => s.askConfirm);
  const pushToast = useApp((s) => s.pushToast);

  const [status, setStatus] = useState<BatchStatus | null>(null);
  const [statusError, setStatusError] = useState('');
  const [form, setForm] = useState({
    namespace: 'bocomwork',
    batchSize: 20,
    wakeWindow: '10m',
    runTimeout: '5m',
    startTime: '',
    deadline: '',
  });
  const [jsonText, setJsonText] = useState(TEMPLATE_JSON);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  const [jsonError, jsonValue] = (() => {
    try {
      const parsed = JSON.parse(jsonText);
      if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
        return ['JSON 根节点必须是对象', null];
      }
      return ['', parsed as Record<string, unknown>];
    } catch (err) {
      return [err instanceof Error ? err.message : 'JSON 格式错误', null];
    }
  })();

  useEffect(() => {
    if (!authenticated) {
      setStatus(null);
      return;
    }
    let stopped = false;
    let timer: ReturnType<typeof setTimeout>;
    const controller = new AbortController();

    async function refresh() {
      try {
        const response = await fetch(apiUrl('api/workspaces/batch-restart'), {
          credentials: 'same-origin',
          signal: controller.signal,
        });
        if (response.status === 401) window.dispatchEvent(new CustomEvent('bw:unauthorized'));
        if (!response.ok) throw new Error(await response.text());
        const next = (await response.json()) as BatchStatus;
        if (!stopped) {
          setStatus(next);
          setStatusError('');
        }
      } catch (err) {
        if (!stopped) setStatusError(err instanceof Error ? err.message : String(err));
      } finally {
        if (!stopped) timer = setTimeout(() => void refresh(), 4000);
      }
    }

    void refresh();
    return () => {
      stopped = true;
      clearTimeout(timer);
      controller.abort();
    };
  }, [authenticated]);

  const active = !!status?.active;
  const processed = status ? status.stopped + status.failed + status.skipped : 0;
  const percent = status && status.total > 0 ? Math.round((processed / status.total) * 100) : 0;
  const stateLabel = STATE_LABELS[status?.state ?? 'none'] ?? status?.state ?? '未知';
  const templateDisabled = busy || active;

  async function request(path: string, init?: RequestInit) {
    const response = await fetch(apiUrl(path), {
      credentials: 'same-origin',
      ...init,
      headers: { 'Content-Type': 'application/json', ...(init?.headers || {}) },
    });
    if (response.status === 401) window.dispatchEvent(new CustomEvent('bw:unauthorized'));
    if (!response.ok) throw new Error(await response.text());
    return response.json();
  }

  function confirmStart() {
    setError('');
    if (jsonError) {
      setError(`模板 JSON 无效：${jsonError}`);
      return;
    }
    if (!jsonValue?.image) {
      setError('模板缺少 image 字段');
      return;
    }
    if (!form.deadline) {
      setError('请设置截止时间（北京时间）');
      return;
    }
    if (form.startTime && form.startTime >= form.deadline) {
      setError('开始时间必须早于截止时间');
      return;
    }
    const startText = form.startTime
      ? `将于 ${form.startTime.replace('T', ' ')}（北京时间）自动开始`
      : '发起后立即开始';
    askConfirm({
      title: '发起批量重启',
      description: `${startText}。将把 ${form.namespace} 下所有 Sleeping/Stopped 工作空间按模板分批拉起（每批 ${form.batchSize} 个），运行 ${form.wakeWindow} 后自动停止。截止时间（北京时间）：${form.deadline.replace('T', ' ')}。个人信息（OA、GUWP_TOKEN 等）自动继承各工作空间原值。`,
      confirmLabel: '发起重启',
      onConfirm: async () => {
        setBusy(true);
        try {
          const started = (await request('api/workspaces/batch-restart', {
            method: 'POST',
            body: JSON.stringify({ ...form, config: jsonValue }),
          })) as BatchStatus;
          setStatus(started);
          pushToast('success', '批量重启已发起', `共 ${started.total} 个目标`);
        } catch (err) {
          const message = err instanceof Error ? err.message : '发起失败';
          setError(message);
          pushToast('error', '发起失败', message);
        } finally {
          setBusy(false);
        }
      },
    });
  }

  function confirmStop() {
    askConfirm({
      title: '停止批量重启',
      description: '停止后将不再拉起新批次，未启动的目标会被跳过；已启动的工作空间会在计时结束后自动停止。',
      confirmLabel: '停止重启',
      tone: 'destructive',
      onConfirm: async () => {
        try {
          const stopped = (await request('api/workspaces/batch-restart', { method: 'DELETE' })) as BatchStatus;
          setStatus(stopped);
          pushToast('info', '已请求停止', '运行中的批次将在计时结束后停止');
        } catch (err) {
          pushToast('error', '停止失败', err instanceof Error ? err.message : '未知错误');
        }
      },
    });
  }

  return (
    <div className="h-full overflow-auto">
      <div className="mx-auto max-w-4xl space-y-6 p-6">
        <header className="space-y-1">
          <p className="text-xs font-medium tracking-widest text-muted-foreground">运维工具</p>
          <div className="flex flex-wrap items-center justify-between gap-2">
            <h1 className="text-xl font-semibold">批量重启</h1>
            {status && (
              <Badge variant="outline" className="gap-1.5 font-mono text-[11px] tabular-nums">
                <Timer className="size-3" aria-hidden="true" />
                服务器时间 {status.serverTime} · {status.timezone}
              </Badge>
            )}
          </div>
          <p className="text-sm text-muted-foreground">
            分批拉起 Sleeping/Stopped 工作空间，促发埋点数据上传，运行一段时间后自动停止
          </p>
        </header>

        {statusError && (
          <p role="alert" className="flex items-center gap-2 rounded-lg border border-destructive/40 bg-destructive/5 p-3 text-sm text-destructive">
            <CircleAlert className="size-4 shrink-0" aria-hidden="true" />
            状态读取失败：{statusError}
          </p>
        )}

        <Section
          icon={<Activity className="size-4" aria-hidden="true" />}
          title="任务进度"
          badge={
            <Badge
              variant={active ? 'default' : 'secondary'}
              className={cn('h-5 text-[10px] font-medium tracking-wide', status?.orphaned && 'border-destructive/40 text-destructive')}
            >
              {status?.orphaned ? '疑似失联' : stateLabel}
            </Badge>
          }
          action={
            active ? (
              <Button variant="destructive" size="sm" onClick={confirmStop}>
                <Square className="size-3.5" aria-hidden="true" />
                停止重启
              </Button>
            ) : undefined
          }
        >
          <div className="space-y-3">
            {status?.orphaned && (
              <p role="alert" className="flex items-start gap-2 rounded-lg border border-destructive/40 bg-destructive/5 p-2.5 text-xs leading-relaxed text-destructive">
                <CircleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
                任务状态长时间未更新，运行该任务的 api-server 副本可能已重启。重新发起可接管剩余目标；已拉起的 Pod 会由自身 idleTimeout 兜底休眠。
              </p>
            )}
            {status && status.state !== 'none' ? (
              <>
                <div className="grid grid-cols-3 gap-2 sm:grid-cols-6">
                  {COUNTERS.map((counter) => (
                    <div key={counter.key} className="rounded-lg border border-border bg-card p-2.5 text-center">
                      <div className={cn('text-lg font-semibold tabular-nums', counter.tone)}>
                        {Number(status[counter.key] ?? 0)}
                      </div>
                      <div className="text-[11px] text-muted-foreground">{counter.label}</div>
                    </div>
                  ))}
                </div>
                <div
                  role="progressbar"
                  aria-valuenow={percent}
                  aria-valuemin={0}
                  aria-valuemax={100}
                  aria-label="重启进度"
                  className="h-2 overflow-hidden rounded-full bg-muted"
                >
                  <div className="h-full rounded-full bg-primary transition-[width] duration-500" style={{ width: `${percent}%` }} />
                </div>
                <p className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
                  <span>进度 {processed}/{status.total}（{percent}%）</span>
                  {status.state === 'waiting' && status.startTime && (
                    <span className="font-medium text-info">定时开始于 {status.startTime}</span>
                  )}
                  {status.deadline && <span>截止（北京时间）{status.deadline}</span>}
                  {status.wakeWindow && <span>运行窗口 {status.wakeWindow}</span>}
                  {status.batchSize && <span>每批 {status.batchSize}</span>}
                  {status.namespace && <span>命名空间 {status.namespace}</span>}
                  {status.startedAt && <span>开始于 {status.startedAt}</span>}
                </p>
                {status.image && (
                  <p className="truncate font-mono text-[11px] text-muted-foreground" title={status.image}>
                    镜像 {status.image}
                  </p>
                )}
                {!!status.failedSamples?.length && (
                  <details className="rounded-lg border border-border bg-card p-2.5">
                    <summary className="cursor-pointer text-xs font-medium text-destructive">
                      失败样例（共 {status.failed} 个，最多展示 {status.failedSamples.length} 条）
                    </summary>
                    <ul className="mt-2 space-y-1">
                      {status.failedSamples.map((sample) => (
                        <li key={sample.name} className="font-mono text-[11px] leading-relaxed text-muted-foreground">
                          {sample.name}
                          {sample.error ? `：${sample.error}` : ''}
                        </li>
                      ))}
                    </ul>
                  </details>
                )}
              </>
            ) : (
              <p className="rounded-lg border border-dashed border-border bg-muted/40 p-3 text-sm text-muted-foreground">
                当前没有批量重启任务。配置编排参数与 Workspace 模板后发起。
              </p>
            )}
          </div>
        </Section>

        <Section
          icon={<Settings2 className="size-4" aria-hidden="true" />}
          title="编排参数"
          badge={active ? <Badge variant="secondary" className="h-5 text-[10px]">任务进行中，参数已锁定</Badge> : undefined}
        >
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
            <Field label="目标命名空间" hint="只处理该命名空间内的 Sleeping/Stopped 工作空间">
              <Input
                value={form.namespace}
                disabled={templateDisabled}
                onChange={(e) => setForm((f) => ({ ...f, namespace: e.target.value }))}
              />
            </Field>
            <Field label="每批数量" hint="同时处于拉起/运行状态的工作空间上限">
              <Input
                type="number"
                min={1}
                max={200}
                value={form.batchSize}
                disabled={templateDisabled}
                onChange={(e) => setForm((f) => ({ ...f, batchSize: Number(e.target.value) || 1 }))}
              />
            </Field>
            <Field label="运行窗口" hint="Pod 就绪后运行多久自动停止，如 10m、30m">
              <Input
                value={form.wakeWindow}
                disabled={templateDisabled}
                onChange={(e) => setForm((f) => ({ ...f, wakeWindow: e.target.value }))}
              />
            </Field>
            <Field label="启动超时" hint="等待 Pod 就绪的最长时间，超时记失败并停止，如 5m">
              <Input
                value={form.runTimeout}
                disabled={templateDisabled}
                onChange={(e) => setForm((f) => ({ ...f, runTimeout: e.target.value }))}
              />
            </Field>
            <Field label="定时开始（北京时间）" hint="到点自动开始；留空则发起后立即执行">
              <Input
                type="datetime-local"
                min={localDatetimeMin()}
                value={form.startTime}
                disabled={templateDisabled}
                onChange={(e) => setForm((f) => ({ ...f, startTime: e.target.value }))}
              />
            </Field>
            <Field label="截止时间（北京时间）" required hint="到点后不再拉起新批次，已启动的照常计时停止">
              <Input
                type="datetime-local"
                required
                min={localDatetimeMin()}
                value={form.deadline}
                disabled={templateDisabled}
                onChange={(e) => setForm((f) => ({ ...f, deadline: e.target.value }))}
              />
            </Field>
          </div>
        </Section>

        <Section
          icon={<FileJson className="size-4" aria-hidden="true" />}
          title="Workspace 模板"
          badge={
            jsonError ? (
              <Badge variant="destructive" className="h-5 gap-1 text-[10px]">
                <CircleAlert className="size-3" aria-hidden="true" />
                JSON 错误
              </Badge>
            ) : (
              <Badge variant="secondary" className="h-5 gap-1 text-[10px] text-success">
                <CircleCheck className="size-3" aria-hidden="true" />
                JSON 有效
              </Badge>
            )
          }
          action={
            <Button disabled={templateDisabled || !!jsonError} onClick={confirmStart}>
              <Play className="size-3.5" aria-hidden="true" />
              {busy ? '正在发起…' : '发起批量重启'}
            </Button>
          }
        >
          <div className="space-y-2">
            <p className="text-xs leading-relaxed text-muted-foreground">
              模板中的字段（镜像、资源、命令、挂载、全局环境变量）会统一覆盖到每个目标；每用户的
              OA、GUWP_TOKEN 等个人信息与模板未列出的环境变量自动继承原值；TZ 未配置时自动注入 Asia/Shanghai。
            </p>
            <div className={cn('rounded-lg border bg-background', jsonError ? 'border-destructive/50' : 'border-border')}>
              <textarea
                value={jsonText}
                onChange={(e) => setJsonText(e.target.value)}
                spellCheck={false}
                wrap="off"
                aria-label="Workspace 模板 JSON"
                aria-invalid={!!jsonError}
                disabled={templateDisabled}
                className="h-96 w-full resize-none rounded-lg bg-transparent p-3 font-mono text-[11.5px] leading-relaxed outline-none placeholder:text-muted-foreground focus-visible:ring-0 disabled:cursor-not-allowed disabled:opacity-50"
              />
            </div>
            {jsonError && (
              <p className="text-[11px] text-destructive" role="alert">
                {jsonError}
              </p>
            )}
            {error && (
              <p className="rounded-lg border border-destructive/40 bg-destructive/5 p-2.5 text-xs text-destructive" role="alert">
                {error}
              </p>
            )}
          </div>
        </Section>
      </div>
    </div>
  );
}
