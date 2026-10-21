import { useEffect } from 'react';
import { AnimatePresence, motion, useReducedMotion } from 'motion/react';
import { CircleAlert, CircleCheck, Info, X } from 'lucide-react';
import { useApp } from '@/store';
import type { Toast } from '@/types';
import { cn } from '@/lib/utils';

export function Toaster() {
  const toasts = useApp((state) => state.toasts);
  const reduce = useReducedMotion();
  return (
    <div
      aria-live="polite"
      aria-atomic="false"
      className="pointer-events-none fixed bottom-3 right-3 z-[60] flex w-[calc(100%-1.5rem)] max-w-sm flex-col gap-2 sm:bottom-4 sm:right-4"
    >
      <AnimatePresence initial={false}>
        {toasts.map((toast) => (
          <ToastItem key={toast.id} toast={toast} reduce={!!reduce} />
        ))}
      </AnimatePresence>
    </div>
  );
}

function ToastItem({ toast, reduce }: { toast: Toast; reduce: boolean }) {
  const dismiss = useApp((state) => state.dismissToast);
  useEffect(() => {
    const timer = window.setTimeout(() => dismiss(toast.id), 4200);
    return () => window.clearTimeout(timer);
  }, [dismiss, toast.id]);

  const tones = {
    success: {
      icon: <CircleCheck className="size-4" />,
      className: 'text-success',
    },
    error: {
      icon: <CircleAlert className="size-4" />,
      className: 'text-destructive',
    },
    info: {
      icon: <Info className="size-4" />,
      className: 'text-info',
    },
  }[toast.tone];

  return (
    <motion.div
      layout
      initial={reduce ? { opacity: 0 } : { opacity: 0, y: 12, scale: 0.97 }}
      animate={{ opacity: 1, y: 0, scale: 1 }}
      exit={reduce ? { opacity: 0 } : { opacity: 0, x: 16, scale: 0.97 }}
      transition={{ type: 'spring', stiffness: 420, damping: 32 }}
      role={toast.tone === 'error' ? 'alert' : 'status'}
      className="pointer-events-auto flex items-start gap-2.5 rounded-lg border border-border bg-popover p-3 pr-2 text-popover-foreground shadow-lg"
    >
      <span className={cn('mt-0.5 shrink-0', tones.className)}>{tones.icon}</span>
      <div className="min-w-0 flex-1">
        <p className="text-[12.5px] font-medium leading-snug">{toast.title}</p>
        {toast.description && (
          <p className="mt-0.5 break-words text-[11px] leading-snug text-muted-foreground">
            {toast.description}
          </p>
        )}
      </div>
      <button
        type="button"
        aria-label="关闭通知"
        onClick={() => dismiss(toast.id)}
        className="grid size-6 shrink-0 place-items-center rounded text-muted-foreground outline-none transition-colors hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/40"
      >
        <X className="size-3" />
      </button>
    </motion.div>
  );
}
