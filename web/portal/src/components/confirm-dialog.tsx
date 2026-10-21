import { useApp } from '@/store';
import { Modal } from '@/components/ui/dialog';
import { Button } from '@/components/ui/button';

export function ConfirmDialog() {
  const confirm = useApp((state) => state.confirm);
  const closeConfirm = useApp((state) => state.closeConfirm);

  const run = () => {
    if (!confirm) return;
    try {
      const action = confirm.onConfirm();
      // Close immediately; the action reports progress through the operation banner.
      closeConfirm();
      void Promise.resolve(action).catch(() => undefined);
    } catch {
      closeConfirm();
    }
  };

  return (
    <Modal
      open={!!confirm}
      onOpenChange={(open) => {
        if (!open) closeConfirm();
      }}
      title={confirm?.title || '操作确认'}
      description={confirm?.description}
      className="max-w-sm"
      footer={
        <>
          <Button variant="outline" onClick={closeConfirm}>
            取消
          </Button>
          <Button
            variant={confirm?.tone === 'destructive' ? 'destructive' : 'default'}
            onClick={run}
          >
            {confirm?.confirmLabel || '确认执行'}
          </Button>
        </>
      }
    />
  );
}
