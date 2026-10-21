import { Badge, type BadgeProps } from '@/components/ui/badge';

type BadgeVariant = NonNullable<BadgeProps['variant']>;

const PHASE_META: Record<string, { label: string; variant: BadgeVariant; pulse?: boolean }> = {
  running: { label: '运行中', variant: 'success', pulse: true },
  starting: { label: '启动中', variant: 'info', pulse: true },
  pending: { label: '等待中', variant: 'info', pulse: true },
  sleeping: { label: '休眠中', variant: 'warning' },
  stopped: { label: '已停止', variant: 'muted' },
  failed: { label: '失败', variant: 'destructive' },
  unknown: { label: '未知', variant: 'outline' },
};

export function phaseMeta(phase?: string) {
  const key = (phase || 'unknown').toLowerCase();
  return PHASE_META[key] || { label: phase || '未知', variant: 'outline' as BadgeVariant };
}

export function StatusBadge({ phase, className }: { phase?: string; className?: string }) {
  const meta = phaseMeta(phase);
  return (
    <Badge variant={meta.variant} className={className} title={`Phase: ${phase || 'Unknown'}`}>
      <span
        className={`size-1.5 rounded-full bg-current ${meta.pulse ? 'status-pulse' : ''}`}
        aria-hidden="true"
      />
      {meta.label}
    </Badge>
  );
}
