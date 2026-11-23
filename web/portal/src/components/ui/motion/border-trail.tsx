import { motion } from 'motion/react';
import { cn } from '@/lib/utils';

export interface BorderTrailProps {
  className?: string;
  size?: number; // length of the trail in percent, default 25
  strokeWidth?: number;
  duration?: number;
  color?: string;
}

export function BorderTrail({
  className,
  size = 25,
  strokeWidth = 2,
  duration = 4,
  color,
}: BorderTrailProps) {
  const dashArray = `${size} ${100 - size}`;

  return (
    <div className="pointer-events-none absolute inset-0 size-full overflow-hidden rounded-[inherit]">
      <svg
        className="size-full overflow-visible"
        xmlns="http://www.w3.org/2000/svg"
        fill="none"
      >
        <motion.rect
          x="1"
          y="1"
          width="calc(100% - 2px)"
          height="calc(100% - 2px)"
          rx="calc(var(--radius) - 1px)"
          pathLength="100"
          strokeDasharray={dashArray}
          strokeLinecap="round"
          stroke={color || 'var(--primary)'}
          strokeWidth={strokeWidth}
          className={cn('opacity-80 drop-shadow-[0_0_8px_var(--primary)]', className)}
          animate={{
            strokeDashoffset: [0, -100],
          }}
          transition={{
            repeat: Infinity,
            duration,
            ease: 'linear',
          }}
        />
      </svg>
    </div>
  );
}
