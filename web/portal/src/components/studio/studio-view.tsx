import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react';
import { AnimatePresence, motion } from 'motion/react';
import {
  Braces,
  Box,
  CircleAlert,
  CircleCheck,
  Copy,
  ExternalLink,
  FileCog,
  FileJson,
  FolderOpen,
  GripVertical,
  HardDrive,
  ListTree,
  Loader2,
  Play,
  Plus,
  Rocket,
  RotateCcw,
  Server,
  Share2,
  Square,
  SquareTerminal,
  Terminal,
  Trash2,
  UserRound,
} from 'lucide-react';
import { Panel, PanelGroup, PanelResizeHandle } from 'react-resizable-panels';
import { useApp } from '@/store';
import type { InspectorTab, Payload } from '@/types';
import { Button } from '@/components/ui/button';
import { Badge } from '@/components/ui/badge';
import { Card } from '@/components/ui/card';
import { Field } from '@/components/ui/field';
import { Input, Textarea } from '@/components/ui/input';
import { Segmented } from '@/components/ui/segmented';
import { StatusBadge } from '@/components/ui/status-badge';
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs';
import { Tip } from '@/components/ui/tooltip';
import { SortableList } from '@/components/studio/sortable-list';
import { CsvInput } from '@/components/studio/csv-input';
import { cn, copyText, safeExternalUrl, uid } from '@/lib/utils';
import { createDefaultPayload, generateCurl, generateYaml } from '@/lib/workspace';
import { useMediaQuery } from '@/lib/use-media-query';

type MobilePane = 'form' | 'inspector';

export function StudioView() {
  const isDesktop = useMediaQuery('(min-width: 1024px)');
  const deploy = useApp((state) => state.deploy);
  const operation = useApp((state) => state.operation);
  const [deploying, setDeploying] = useState(false);
  const [mobilePane, setMobilePane] = useState<MobilePane>('form');
  const payload = useApp((state) => state.payload);
  const checkTarget = useApp((state) => state.checkTarget);
  const auth = useApp((state) => state.auth);

  useEffect(() => {
    const timer = window.setTimeout(() => void checkTarget(), 450);
    return () => window.clearTimeout(timer);
  }, [payload.userId, payload.namespace, checkTarget]);

  const handleDeploy = useCallback(async () => {
    if (deploying || operation) return;
    setDeploying(true);
    try {
      await deploy();
    } finally {
      setDeploying(false);
    }
  }, [deploy, deploying, operation]);

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && event.key === 'Enter' && !auth.loginOpen) {
        event.preventDefault();
        void handleDeploy();
      }
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, [auth.loginOpen, handleDeploy]);

  if (!isDesktop) {
    return (
      <div className="flex h-full min-h-0 flex-col">
        <div className="flex items-center justify-between gap-2 border-b border-border bg-card/40 px-3 py-2">
          <Segmented
            value={mobilePane}
            options={[
              { value: 'form', label: '配置', icon: <FileCog className="size-3.5" /> },
              { value: 'inspector', label: '预览与部署', icon: <Braces className="size-3.5" /> },
            ]}
            onChange={setMobilePane}
            layoutId="studio-mobile-pane"
            ariaLabel="切换配置或预览"
          />
          <Badge variant="outline" className="font-mono text-[10px]">
            {payload.userId || '未命名'}
          </Badge>
        </div>
        <div className="min-h-0 flex-1">
          {mobilePane === 'form' ? (
            <FormPanel />
          ) : (
            <InspectorPanel deploying={deploying} onDeploy={handleDeploy} />
          )}
        </div>
      </div>
    );
  }

  return (
    <PanelGroup
      direction="horizontal"
      autoSaveId="bw-studio-layout-v2"
      className="h-full min-h-0"
      keyboardResizeBy={2}
    >
      <Panel defaultSize={54} minSize={34} maxSize={74} className="min-h-0 min-w-0">
        <FormPanel />
      </Panel>
      <PanelResizeHandle className="panel-handle" aria-label="调整配置与预览面板宽度">
        <span className="panel-handle-grip" aria-hidden="true">
          <GripVertical className="size-3" />
        </span>
      </PanelResizeHandle>
      <Panel minSize={26} className="min-h-0 min-w-0">
        <InspectorPanel deploying={deploying} onDeploy={handleDeploy} />
      </Panel>
    </PanelGroup>
  );
}

function FormPanel() {
  const payload = useApp((state) => state.payload);
  const updateField = useApp((state) => state.updateField);
  const addItem = useApp((state) => state.addItem);
  const updateItem = useApp((state) => state.updateItem);
  const removeItem = useApp((state) => state.removeItem);
  const reorderItems = useApp((state) => state.reorderItems);
  const fieldErrors = useApp((state) => state.fieldErrors);
  const setPayload = useApp((state) => state.setPayload);
  const askConfirm = useApp((state) => state.askConfirm);

  const resetDraft = () => {
    askConfirm({
      title: '重置编排草稿',
      description: '当前表单中的修改将全部丢弃，并恢复为默认配置。',
      confirmLabel: '重置草稿',
      tone: 'destructive',
      onConfirm: () => setPayload(createDefaultPayload()),
    });
  };

  return (
    <div className="h-full overflow-y-auto overscroll-contain bg-background">
      <div className="flex flex-col gap-3 p-3 pb-24 sm:p-5 lg:pb-8">
        <div className="flex items-start justify-between gap-3">
          <div>
            <h1 className="text-base font-semibold tracking-tight">编排与设计器</h1>
            <p className="mt-0.5 text-xs text-muted-foreground">
              组合运行规格、存储与注入配置，右侧实时预览 API 负载。
            </p>
          </div>
          <Button variant="ghost" size="sm" onClick={resetDraft}>
            <RotateCcw className="size-3.5" />
            重置
          </Button>
        </div>

        <Section
          icon={<UserRound className="size-3.5" />}
          title="基础标识"
          description="工作空间归属与目标命名空间"
          badge="必填"
        >
          <div className="grid gap-3 sm:grid-cols-2">
            <Field
              label="用户 ID"
              htmlFor="field-user-id"
              required
              hint="作为工作空间所有者的唯一标识"
              error={fieldErrors.userId}
            >
              <Input
                id="field-user-id"
                value={payload.userId}
                onChange={(event) => updateField('userId', event.target.value)}
                placeholder="sun_yunfeng"
                autoComplete="off"
                spellCheck={false}
                aria-invalid={!!fieldErrors.userId}
              />
            </Field>
            <Field
              label="命名空间"
              htmlFor="field-namespace"
              hint="K8s 目标命名空间，默认 bocomwork"
            >
              <Input
                id="field-namespace"
                value={payload.namespace}
                onChange={(event) => updateField('namespace', event.target.value)}
                placeholder="bocomwork"
                autoComplete="off"
                spellCheck={false}
                className="font-mono text-xs"
              />
            </Field>
          </div>
        </Section>

        <Section
          icon={<Box className="size-3.5" />}
          title="运行镜像与计算规格"
          description="容器入口、端口与资源上限"
          badge="Core"
        >
          <div className="flex flex-col gap-3">
            <Field
              label="容器镜像"
              htmlFor="field-image"
              required
              error={fieldErrors.image}
            >
              <Input
                id="field-image"
                value={payload.image}
                onChange={(event) => updateField('image', event.target.value)}
                placeholder="docker.io/library/bocomwork:v1.0.2"
                spellCheck={false}
                aria-invalid={!!fieldErrors.image}
                className="font-mono text-xs"
              />
            </Field>
            <div className="grid gap-3 sm:grid-cols-3">
              <Field label="服务端口" htmlFor="field-port" error={fieldErrors.port}>
                <Input
                  id="field-port"
                  type="number"
                  min={1}
                  max={65535}
                  value={payload.port || ''}
                  onChange={(event) => updateField('port', Number(event.target.value) || 0)}
                  placeholder="3000"
                  aria-invalid={!!fieldErrors.port}
                  className="font-mono text-xs"
                />
              </Field>
              <Field label="CPU 限制" htmlFor="field-cpu" hint="如 0.5 / 2">
                <Input
                  id="field-cpu"
                  value={payload.cpu}
                  onChange={(event) => updateField('cpu', event.target.value)}
                  placeholder="0.5"
                  spellCheck={false}
                  className="font-mono text-xs"
                />
              </Field>
              <Field label="内存限制" htmlFor="field-memory" hint="如 1Gi / 4Gi">
                <Input
                  id="field-memory"
                  value={payload.memory}
                  onChange={(event) => updateField('memory', event.target.value)}
                  placeholder="1Gi（默认）"
                  spellCheck={false}
                  className="font-mono text-xs"
                />
              </Field>
            </div>
            <div className="grid gap-3 sm:grid-cols-2">
              <Field
                label="健康检查路径"
                htmlFor="field-health"
                hint="HTTP 就绪探针；留空则使用 TCP 探测"
              >
                <Input
                  id="field-health"
                  value={payload.healthPath}
                  onChange={(event) => updateField('healthPath', event.target.value)}
                  placeholder="/health"
                  spellCheck={false}
                  className="font-mono text-xs"
                />
              </Field>
              <Field
                label="空闲休眠超时"
                htmlFor="field-idle"
                hint="数字 + h/m/s/ms 的组合"
                error={fieldErrors.idleTimeout}
              >
                <Input
                  id="field-idle"
                  value={payload.idleTimeout}
                  onChange={(event) => updateField('idleTimeout', event.target.value)}
                  placeholder="10m"
                  spellCheck={false}
                  aria-invalid={!!fieldErrors.idleTimeout}
                  className="font-mono text-xs"
                />
              </Field>
            </div>
          </div>
        </Section>

        <Section
          icon={<HardDrive className="size-3.5" />}
          title="持久化存储"
          description="用户工作目录的 PVC 规格"
        >
          <div className="grid gap-3 sm:grid-cols-2">
            <Field
              label="存储容量"
              htmlFor="field-storage-size"
              hint="留空使用服务端默认 1Gi"
              error={fieldErrors.storageSize}
            >
              <Input
                id="field-storage-size"
                value={payload.storageSize}
                onChange={(event) => updateField('storageSize', event.target.value)}
                placeholder="1Gi（默认）"
                spellCheck={false}
                aria-invalid={!!fieldErrors.storageSize}
                className="font-mono text-xs"
              />
            </Field>
            <Field
              label="StorageClass"
              htmlFor="field-storage-class"
              hint="留空使用集群默认 StorageClass"
            >
              <Input
                id="field-storage-class"
                value={payload.storageClass}
                onChange={(event) => updateField('storageClass', event.target.value)}
                placeholder="alicloud-disk-essd"
                spellCheck={false}
                className="font-mono text-xs"
              />
            </Field>
          </div>
        </Section>

        <Section
          icon={<FolderOpen className="size-3.5" />}
          title="用户持久化挂载"
          description="映射到工作空间 PVC 的目录"
          badge={String(payload.volumeMounts.length)}
          action={
            <Button
              variant="outline"
              size="sm"
              onClick={() => addItem('volumeMounts', { id: uid('vol'), mountPath: '', subPath: '' })}
            >
              <Plus className="size-3.5" />
              添加挂载
            </Button>
          }
        >
          <SortableList
            items={payload.volumeMounts}
            ariaLabel="用户持久化挂载"
            emptyLabel="尚未添加挂载点"
            onReorder={(activeId, overId) => reorderItems('volumeMounts', activeId, overId)}
          >
            {(item) => (
              <>
                <div className="grid flex-1 gap-2 sm:grid-cols-2">
                  <Input
                    aria-label="容器内挂载路径"
                    className="font-mono text-xs"
                    value={item.mountPath}
                    onChange={(event) =>
                      updateItem('volumeMounts', item.id, { mountPath: event.target.value })
                    }
                    placeholder="/root/.bocomwork"
                    spellCheck={false}
                  />
                  <Input
                    aria-label="子路径"
                    className="font-mono text-xs"
                    value={item.subPath}
                    onChange={(event) =>
                      updateItem('volumeMounts', item.id, { subPath: event.target.value })
                    }
                    placeholder="subPath（可选）"
                    spellCheck={false}
                  />
                </div>
                <RemoveButton
                  label="删除挂载点"
                  onClick={() => removeItem('volumeMounts', item.id)}
                />
              </>
            )}
          </SortableList>
        </Section>

        <Section
          icon={<Share2 className="size-3.5" />}
          title="共享存储挂载"
          description="挂载集群中已存在的共享 PVC"
          badge={String(payload.sharedVolumeMounts.length)}
          action={
            <Button
              variant="outline"
              size="sm"
              onClick={() =>
                addItem('sharedVolumeMounts', {
                  id: uid('shared'),
                  pvcName: '',
                  mountPath: '',
                  subPath: '',
                  readOnly: false,
                })
              }
            >
              <Plus className="size-3.5" />
              添加共享卷
            </Button>
          }
        >
          <SortableList
            items={payload.sharedVolumeMounts}
            ariaLabel="共享存储挂载"
            emptyLabel="尚未添加共享卷"
            onReorder={(activeId, overId) => reorderItems('sharedVolumeMounts', activeId, overId)}
          >
            {(item) => (
              <>
                <div className="grid flex-1 gap-2 sm:grid-cols-3">
                  <Input
                    aria-label="PVC 名称"
                    className="font-mono text-xs"
                    value={item.pvcName}
                    onChange={(event) =>
                      updateItem('sharedVolumeMounts', item.id, { pvcName: event.target.value })
                    }
                    placeholder="shared-pvc-name"
                    spellCheck={false}
                  />
                  <Input
                    aria-label="挂载路径"
                    className="font-mono text-xs"
                    value={item.mountPath}
                    onChange={(event) =>
                      updateItem('sharedVolumeMounts', item.id, { mountPath: event.target.value })
                    }
                    placeholder="/opt/shared"
                    spellCheck={false}
                  />
                  <Input
                    aria-label="子路径"
                    className="font-mono text-xs"
                    value={item.subPath}
                    onChange={(event) =>
                      updateItem('sharedVolumeMounts', item.id, { subPath: event.target.value })
                    }
                    placeholder="subPath（可选）"
                    spellCheck={false}
                  />
                </div>
                <div className="flex items-center justify-between gap-2 sm:justify-end">
                  <ReadOnlyCheckbox
                    checked={item.readOnly}
                    onChange={(checked) =>
                      updateItem('sharedVolumeMounts', item.id, { readOnly: checked })
                    }
                  />
                  <RemoveButton
                    label="删除共享卷"
                    onClick={() => removeItem('sharedVolumeMounts', item.id)}
                  />
                </div>
              </>
            )}
          </SortableList>
        </Section>

        <Section
          icon={<FileCog className="size-3.5" />}
          title="ConfigMap 挂载"
          description="把配置字典挂载为文件"
          badge={String(payload.configMapVolumeMounts.length)}
          action={
            <Button
              variant="outline"
              size="sm"
              onClick={() =>
                addItem('configMapVolumeMounts', {
                  id: uid('cm'),
                  configMapName: '',
                  mountPath: '',
                  subPath: '',
                  readOnly: false,
                })
              }
            >
              <Plus className="size-3.5" />
              添加配置字典
            </Button>
          }
        >
          <SortableList
            items={payload.configMapVolumeMounts}
            ariaLabel="ConfigMap 挂载"
            emptyLabel="尚未添加 ConfigMap"
            onReorder={(activeId, overId) =>
              reorderItems('configMapVolumeMounts', activeId, overId)
            }
          >
            {(item) => (
              <>
                <div className="grid flex-1 gap-2 sm:grid-cols-3">
                  <Input
                    aria-label="ConfigMap 名称"
                    className="font-mono text-xs"
                    value={item.configMapName}
                    onChange={(event) =>
                      updateItem('configMapVolumeMounts', item.id, {
                        configMapName: event.target.value,
                      })
                    }
                    placeholder="configmap-name"
                    spellCheck={false}
                  />
                  <Input
                    aria-label="挂载路径"
                    className="font-mono text-xs"
                    value={item.mountPath}
                    onChange={(event) =>
                      updateItem('configMapVolumeMounts', item.id, {
                        mountPath: event.target.value,
                      })
                    }
                    placeholder="/etc/config"
                    spellCheck={false}
                  />
                  <Input
                    aria-label="子路径"
                    className="font-mono text-xs"
                    value={item.subPath}
                    onChange={(event) =>
                      updateItem('configMapVolumeMounts', item.id, { subPath: event.target.value })
                    }
                    placeholder="key/subPath"
                    spellCheck={false}
                  />
                </div>
                <div className="flex items-center justify-between gap-2 sm:justify-end">
                  <ReadOnlyCheckbox
                    checked={item.readOnly}
                    onChange={(checked) =>
                      updateItem('configMapVolumeMounts', item.id, { readOnly: checked })
                    }
                  />
                  <RemoveButton
                    label="删除 ConfigMap"
                    onClick={() => removeItem('configMapVolumeMounts', item.id)}
                  />
                </div>
              </>
            )}
          </SortableList>
        </Section>

        <Section
          icon={<ListTree className="size-3.5" />}
          title="环境变量"
          description="注入主容器的 KEY / VALUE"
          badge={String(payload.env.length)}
          action={
            <Button
              variant="outline"
              size="sm"
              onClick={() => addItem('env', { id: uid('env'), name: '', value: '' })}
            >
              <Plus className="size-3.5" />
              添加变量
            </Button>
          }
        >
          <SortableList
            items={payload.env}
            ariaLabel="环境变量"
            emptyLabel="尚未添加环境变量"
            onReorder={(activeId, overId) => reorderItems('env', activeId, overId)}
          >
            {(item) => (
              <>
                <div className="grid flex-1 gap-2 sm:grid-cols-[minmax(0,0.8fr)_minmax(0,1.6fr)]">
                  <Input
                    aria-label="变量名"
                    className="font-mono text-xs"
                    value={item.name}
                    onChange={(event) =>
                      updateItem('env', item.id, { name: event.target.value })
                    }
                    placeholder="KEY"
                    spellCheck={false}
                  />
                  <Input
                    aria-label="变量值"
                    className="font-mono text-xs"
                    value={item.value}
                    onChange={(event) =>
                      updateItem('env', item.id, { value: event.target.value })
                    }
                    placeholder="value"
                    spellCheck={false}
                  />
                </div>
                <RemoveButton label="删除变量" onClick={() => removeItem('env', item.id)} />
              </>
            )}
          </SortableList>
        </Section>

        <Section
          icon={<SquareTerminal className="size-3.5" />}
          title="初始化容器"
          description="主容器启动前执行的一次性任务"
          badge={String(payload.initContainers.length)}
          action={
            <Button
              variant="outline"
              size="sm"
              onClick={() =>
                addItem('initContainers', {
                  id: uid('init'),
                  name: '',
                  image: '',
                  command: [],
                  args: [],
                })
              }
            >
              <Plus className="size-3.5" />
              添加 Init 容器
            </Button>
          }
        >
          <SortableList
            items={payload.initContainers}
            ariaLabel="初始化容器"
            emptyLabel="尚未添加初始化容器"
            onReorder={(activeId, overId) => reorderItems('initContainers', activeId, overId)}
          >
            {(item, index) => (
              <>
                <div className="grid flex-1 gap-2 sm:grid-cols-2">
                  <Input
                    aria-label="Init 容器名称"
                    className="font-mono text-xs"
                    value={item.name}
                    onChange={(event) =>
                      updateItem('initContainers', item.id, { name: event.target.value })
                    }
                    placeholder="容器名称"
                    spellCheck={false}
                  />
                  <Input
                    aria-label="Init 容器镜像"
                    className="font-mono text-xs"
                    value={item.image}
                    onChange={(event) =>
                      updateItem('initContainers', item.id, { image: event.target.value })
                    }
                    placeholder="镜像"
                    spellCheck={false}
                  />
                  <CsvInput
                    ariaLabel="Init 容器启动命令"
                    className="font-mono text-xs"
                    value={item.command}
                    onChange={(value) =>
                      updateItem('initContainers', item.id, { command: value })
                    }
                    placeholder="Command，逗号分隔"
                  />
                  <CsvInput
                    ariaLabel="Init 容器启动参数"
                    className="font-mono text-xs"
                    value={item.args}
                    onChange={(value) => updateItem('initContainers', item.id, { args: value })}
                    placeholder="Args，逗号分隔"
                  />
                  {fieldErrors[`init-${index}`] && (
                    <p className="text-[11px] text-destructive sm:col-span-2" role="alert">
                      {fieldErrors[`init-${index}`]}
                    </p>
                  )}
                </div>
                <RemoveButton
                  label="删除 Init 容器"
                  onClick={() => removeItem('initContainers', item.id)}
                />
              </>
            )}
          </SortableList>
        </Section>

        <Section
          icon={<Terminal className="size-3.5" />}
          title="启动命令与生命周期"
          description="Entrypoint、Args 与 PostStart 脚本"
        >
          <div className="flex flex-col gap-3">
            <div className="grid gap-3 sm:grid-cols-2">
              <Field label="启动命令" htmlFor="field-command" hint="逗号分隔，例如 bocomwork-entrypoint">
                <CsvInput
                  id="field-command"
                  ariaLabel="启动命令"
                  className="font-mono text-xs"
                  value={payload.command}
                  onChange={(value) => updateField('command', value)}
                  placeholder="bocomwork-entrypoint"
                />
              </Field>
              <Field label="启动参数" htmlFor="field-args" hint="逗号分隔，可留空">
                <CsvInput
                  id="field-args"
                  ariaLabel="启动参数"
                  className="font-mono text-xs"
                  value={payload.args}
                  onChange={(value) => updateField('args', value)}
                  placeholder="--verbose, --port=3000"
                />
              </Field>
            </div>
            <Field
              label="PostStart 脚本"
              htmlFor="field-post-start"
              hint="容器启动后执行的 shell 片段"
            >
              <Textarea
                id="field-post-start"
                value={payload.postStartScript}
                onChange={(event) => updateField('postStartScript', event.target.value)}
                placeholder="echo 'Container initialized' && touch /workspace/boot-success"
                spellCheck={false}
                rows={4}
                className="min-h-24 resize-y font-mono text-xs leading-relaxed"
              />
            </Field>
          </div>
        </Section>
      </div>
    </div>
  );
}

function Section({
  icon,
  title,
  description,
  badge,
  action,
  children,
}: {
  icon: ReactNode;
  title: string;
  description?: string;
  badge?: string;
  action?: ReactNode;
  children: ReactNode;
}) {
  return (
    <Card className="overflow-hidden">
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border bg-muted/25 px-3.5 py-2.5">
        <div className="flex min-w-0 items-center gap-2.5">
          <span className="grid size-7 shrink-0 place-items-center rounded-md border border-border bg-background text-muted-foreground">
            {icon}
          </span>
          <div className="min-w-0">
            <div className="flex items-center gap-2">
              <h2 className="truncate text-[13px] font-semibold">{title}</h2>
              {badge && (
                <Badge variant="secondary" className="h-5 text-[10px] tabular-nums">
                  {badge}
                </Badge>
              )}
            </div>
            {description && (
              <p className="truncate text-[11px] text-muted-foreground">{description}</p>
            )}
          </div>
        </div>
        {action && <div className="ml-auto">{action}</div>}
      </div>
      <div className="p-3.5">{children}</div>
    </Card>
  );
}

function RemoveButton({ label, onClick }: { label: string; onClick: () => void }) {
  return (
    <Tip label={label}>
      <Button
        variant="ghost"
        size="iconSm"
        onClick={onClick}
        aria-label={label}
        className="shrink-0 text-muted-foreground hover:text-destructive"
      >
        <Trash2 className="size-3.5" />
      </Button>
    </Tip>
  );
}


function ReadOnlyCheckbox({
  checked,
  onChange,
}: {
  checked: boolean;
  onChange: (checked: boolean) => void;
}) {
  return (
    <label className="inline-flex cursor-pointer select-none items-center gap-1.5 text-[11px] text-muted-foreground">
      <input
        type="checkbox"
        checked={checked}
        onChange={(event) => onChange(event.target.checked)}
        className="size-3.5 rounded border-input accent-primary"
      />
      只读
    </label>
  );
}

function InspectorPanel({
  deploying,
  onDeploy,
}: {
  deploying: boolean;
  onDeploy: () => void;
}) {
  const payload = useApp((state) => state.payload);
  const target = useApp((state) => state.target);
  const inspectorTab = useApp((state) => state.inspectorTab);
  const setInspectorTab = useApp((state) => state.setInspectorTab);
  const jsonDraft = useApp((state) => state.jsonDraft);
  const jsonError = useApp((state) => state.jsonError);
  const setJsonDraft = useApp((state) => state.setJsonDraft);
  const resetJsonDraft = useApp((state) => state.resetJsonDraft);
  const deployResult = useApp((state) => state.deployResult);
  const operation = useApp((state) => state.operation);
  const askConfirm = useApp((state) => state.askConfirm);
  const stopTarget = useApp((state) => state.stopTarget);
  const wakeupTarget = useApp((state) => state.wakeupTarget);
  const pushToast = useApp((state) => state.pushToast);

  const yaml = useMemo(() => generateYaml(payload), [payload]);
  const curl = useMemo(() => generateCurl(payload), [payload]);
  const code = inspectorTab === 'json' ? jsonDraft : inspectorTab === 'yaml' ? yaml : curl;

  const url = safeExternalUrl(target.url);
  const phase = (target.phase || '').toLowerCase();

  const copyCode = async () => {
    const ok = await copyText(code);
    pushToast(ok ? 'success' : 'error', ok ? '已复制到剪贴板' : '复制失败', ok ? undefined : '请手动选择文本复制');
  };

  const confirmStop = () => {
    askConfirm({
      title: '停止工作空间',
      description: `确定要停止用户 ${payload.userId || '当前用户'} 的工作空间吗？`,
      confirmLabel: '停止实例',
      tone: 'destructive',
      onConfirm: () => stopTarget(),
    });
  };

  return (
    <div className="flex h-full min-h-0 flex-col border-l border-border bg-card">
      <div className="p-2.5 pb-0">
        <div className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-border bg-muted/25 p-2.5">
          <div className="flex min-w-0 items-center gap-2.5">
            <span className="grid size-8 shrink-0 place-items-center rounded-md border border-border bg-background text-muted-foreground">
              <Server className="size-4" />
            </span>
            <div className="min-w-0">
              <div className="flex items-center gap-2">
                {payload.userId ? (
                  target.exists ? (
                    <StatusBadge phase={target.phase} />
                  ) : target.loading ? (
                    <Badge variant="outline">检查中</Badge>
                  ) : (
                    <Badge variant="outline">未部署</Badge>
                  )
                ) : (
                  <Badge variant="outline">未填写用户</Badge>
                )}
                {target.loading && <Loader2 className="size-3 animate-spin text-muted-foreground" />}
                <span className="truncate text-xs font-medium">
                  {payload.userId || '未选择目标'}
                </span>
              </div>
              <div className="truncate font-mono text-[10px] text-muted-foreground">
                {payload.namespace || 'bocomwork'} · {payload.image || '未填写镜像'}
              </div>
            </div>
          </div>

          <div className="flex items-center gap-1">
            {phase === 'running' && url && (
              <a
                href={url}
                target="_blank"
                rel="noreferrer noopener"
                className={cn(
                  'inline-flex h-7 items-center gap-1.5 rounded-md border border-border bg-background px-2 text-[11px] font-medium outline-none transition-colors hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/40',
                )}
              >
                <ExternalLink className="size-3" />
                进入
              </a>
            )}
            {phase === 'running' && (
              <Tip label="停止实例">
                <Button
                  variant="ghost"
                  size="iconSm"
                  onClick={confirmStop}
                  disabled={!!operation}
                  aria-label="停止实例"
                >
                  <Square className="size-3.5 text-destructive" />
                </Button>
              </Tip>
            )}
            {(phase === 'sleeping' || phase === 'stopped') && (
              <Button
                size="sm"
                onClick={() => void wakeupTarget()}
                disabled={!!operation}
                className="h-7 text-[11px]"
              >
                <Play className="size-3" />
                唤醒
              </Button>
            )}
            {phase === 'failed' && (
              <Button
                size="sm"
                onClick={onDeploy}
                disabled={!!operation || deploying}
                className="h-7 text-[11px]"
              >
                <RotateCcw className="size-3" />
                重新部署
              </Button>
            )}
          </div>
        </div>
      </div>

      <Tabs
        value={inspectorTab}
        onValueChange={(value) => setInspectorTab(value as InspectorTab)}
        className="flex min-h-0 flex-1 flex-col"
      >
        <div className="flex flex-wrap items-center justify-between gap-2 px-2.5 py-2">
          <TabsList aria-label="负载预览格式">
            <TabsTrigger value="json">
              <FileJson className="size-3" />
              JSON
            </TabsTrigger>
            <TabsTrigger value="yaml">
              <FileCog className="size-3" />
              YAML
            </TabsTrigger>
            <TabsTrigger value="curl">
              <SquareTerminal className="size-3" />
              cURL
            </TabsTrigger>
          </TabsList>
          <div className="flex items-center gap-1.5">
            {inspectorTab === 'json' &&
              (jsonError ? (
                <span className="hidden items-center gap-1 text-[10px] text-destructive sm:inline-flex">
                  <CircleAlert className="size-3" />
                  JSON 错误
                </span>
              ) : (
                <span className="hidden items-center gap-1 text-[10px] text-success sm:inline-flex">
                  <CircleCheck className="size-3" />
                  JSON 有效
                </span>
              ))}
            <Tip label="复制当前内容">
              <Button variant="ghost" size="sm" onClick={() => void copyCode()} className="h-7 gap-1.5 px-2 text-[11px]">
                <Copy className="size-3" />
                复制
              </Button>
            </Tip>
          </div>
        </div>

        <TabsContent
          value={inspectorTab}
          className="flex min-h-0 flex-1 flex-col px-2.5 outline-none"
        >
          <div
            className={cn(
              'relative flex min-h-0 flex-1 flex-col overflow-hidden rounded-lg border bg-background',
              jsonError && inspectorTab === 'json' ? 'border-destructive/50' : 'border-border',
            )}
          >
            <textarea
              value={code}
              onChange={(event) => {
                if (inspectorTab === 'json') setJsonDraft(event.target.value);
              }}
              readOnly={inspectorTab !== 'json'}
              spellCheck={false}
              wrap="off"
              aria-label={`${inspectorTab.toUpperCase()} 预览`}
              aria-invalid={inspectorTab === 'json' && !!jsonError}
              className="h-full min-h-[240px] w-full resize-none bg-transparent p-3 font-mono text-[11.5px] leading-relaxed outline-none placeholder:text-muted-foreground read-only:text-muted-foreground focus-visible:ring-0"
            />
            <div className="pointer-events-none absolute right-2 top-1.5 rounded border border-border bg-card/80 px-1.5 py-0.5 font-mono text-[9px] uppercase text-muted-foreground">
              {inspectorTab}
            </div>
          </div>
          {inspectorTab === 'json' && jsonError && (
            <div className="flex items-start justify-between gap-2 px-1 py-1.5">
              <p className="text-[10px] leading-snug text-destructive" role="alert">
                {jsonError}
              </p>
              <button
                type="button"
                onClick={() => resetJsonDraft()}
                className="shrink-0 text-[10px] text-muted-foreground underline-offset-2 hover:text-foreground hover:underline"
              >
                丢弃修改
              </button>
            </div>
          )}
        </TabsContent>
      </Tabs>

      <div className="border-t border-border bg-card p-2.5">
        <AnimatePresence initial={false}>
          {deployResult && (
            <motion.div
              initial={{ opacity: 0, height: 0, marginBottom: 0 }}
              animate={{ opacity: 1, height: 'auto', marginBottom: 8 }}
              exit={{ opacity: 0, height: 0, marginBottom: 0 }}
              className="overflow-hidden"
            >
              <div
                className={cn(
                  'flex items-center justify-between gap-2 rounded-md border px-2.5 py-2 text-[11px]',
                  deployResult.tone === 'success'
                    ? 'border-success/25 bg-success/8 text-success'
                    : 'border-destructive/25 bg-destructive/8 text-destructive',
                )}
              >
                <span className="flex min-w-0 items-center gap-1.5">
                  {deployResult.tone === 'success' ? (
                    <CircleCheck className="size-3.5 shrink-0" />
                  ) : (
                    <CircleAlert className="size-3.5 shrink-0" />
                  )}
                  <span className="truncate">{deployResult.message}</span>
                </span>
                {deployResult.tone === 'success' && safeExternalUrl(deployResult.url) && (
                  <a
                    href={safeExternalUrl(deployResult.url)}
                    target="_blank"
                    rel="noreferrer noopener"
                    className="inline-flex shrink-0 items-center gap-1 underline-offset-4 hover:underline"
                  >
                    打开
                    <ExternalLink className="size-3" />
                  </a>
                )}
              </div>
            </motion.div>
          )}
        </AnimatePresence>
        {jsonError && (
          <p className="mb-1.5 text-[10px] leading-snug text-destructive" role="status">
            JSON 草稿存在语法错误，修正或丢弃后才能部署。
          </p>
        )}
        <Button
          size="lg"
          className="w-full"
          onClick={onDeploy}
          disabled={deploying || !!operation || !!jsonError}
          title={jsonError ? '请先修正右侧 JSON 语法错误' : undefined}
        >
          {deploying ? <Loader2 className="size-4 animate-spin" /> : <Rocket className="size-4" />}
          {deploying ? '正在部署…' : '部署 / 更新工作空间'}
        </Button>
        <p className="mt-1.5 text-center text-[10px] text-muted-foreground">
          ⌘ / Ctrl + Enter 快速部署
        </p>
      </div>
    </div>
  );
}
