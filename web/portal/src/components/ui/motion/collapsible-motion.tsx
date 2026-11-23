import { AnimatePresence, motion, useReducedMotion } from 'motion/react';
import { cn } from '@/lib/utils';
import type { ReactNode } from 'react';

export interface CollapsibleMotionProps {
  open: boolean;
  children: ReactNode;
  className?: string;
  duration?: number;
}

export function CollapsibleMotion({
  open,
  children,
  className,
  duration = 0.24,
}: CollapsibleMotionProps) {
  const reduce = useReducedMotion();

  return (
    <AnimatePresence initial={false}>
      {open && (
        <motion.div
          initial={reduce ? { opacity: 0 } : { height: 0, opacity: 0 }}
          animate={reduce ? { opacity: 1 } : { height: 'auto', opacity: 1 }}
          exit={reduce ? { opacity: 0 } : { height: 0, opacity: 0 }}
          transition={{ duration, ease: [0.16, 1, 0.3, 1] }}
          className={cn('overflow-hidden', className)}
        >
          {children}
        </motion.div>
      )}
    </AnimatePresence>
  );
}
