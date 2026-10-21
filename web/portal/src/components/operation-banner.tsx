import { AnimatePresence, motion, useReducedMotion } from 'motion/react';
import { Loader2 } from 'lucide-react';
import { useApp } from '@/store';

export function OperationBanner() {
  const operation = useApp((state) => state.operation);
  const reduce = useReducedMotion();
  return (
    <div className="pointer-events-none fixed inset-x-0 top-[6.6rem] z-50 flex justify-center px-3 md:top-[4.25rem]">
      <AnimatePresence>
        {operation && (
          <motion.div
            initial={reduce ? { opacity: 0 } : { opacity: 0, y: -8, scale: 0.98 }}
            animate={{ opacity: 1, y: 0, scale: 1 }}
            exit={reduce ? { opacity: 0 } : { opacity: 0, y: -6, scale: 0.98 }}
            transition={{ type: 'spring', stiffness: 420, damping: 32 }}
            className="pointer-events-auto flex max-w-md items-center gap-2.5 rounded-lg border border-border bg-popover/95 px-3 py-2 text-popover-foreground shadow-lg backdrop-blur"
            role="status"
            aria-live="polite"
          >
            <Loader2 className="size-3.5 shrink-0 animate-spin text-info" />
            <div className="min-w-0">
              <p className="truncate text-xs font-medium">{operation.title}</p>
              {operation.detail && (
                <p className="truncate text-[10px] text-muted-foreground">{operation.detail}</p>
              )}
            </div>
          </motion.div>
        )}
      </AnimatePresence>
    </div>
  );
}
