import { useEffect } from 'react';
import { motion, useSpring, useTransform, useReducedMotion } from 'motion/react';
import { cn } from '@/lib/utils';

export interface AnimatedProgressProps {
  value: number; // 0 to 100
  max?: number;
  className?: string;
  indicatorClassName?: string;
  showShimmer?: boolean;
}

export function AnimatedProgress({
  value,
  max = 100,
  className,
  indicatorClassName,
  showShimmer = true,
}: AnimatedProgressProps) {
  const reduce = useReducedMotion();
  const percentage = Math.min(100, Math.max(0, (value / max) * 100));

  const spring = useSpring(percentage, {
    stiffness: 120,
    damping: 24,
  });

  const width = useTransform(spring, (latest) => `${latest}%`);

  useEffect(() => {
    spring.set(percentage);
  }, [percentage, spring]);

  return (
    <div
      role="progressbar"
      aria-valuenow={Math.round(percentage)}
      aria-valuemin={0}
      aria-valuemax={max}
      className={cn('relative h-2 w-full overflow-hidden rounded-full bg-muted', className)}
    >
      <motion.div
        className={cn(
          'relative h-full rounded-full bg-primary transition-colors',
          indicatorClassName,
        )}
        style={{ width: reduce ? `${percentage}%` : width }}
      >
        {showShimmer && percentage > 0 && percentage < 100 && (
          <motion.div
            className="absolute inset-0 bg-gradient-to-r from-transparent via-white/30 to-transparent"
            animate={{ x: ['-100%', '100%'] }}
            transition={{ repeat: Infinity, duration: 1.6, ease: 'linear' }}
          />
        )}
      </motion.div>
    </div>
  );
}
