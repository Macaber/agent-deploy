import * as React from 'react';
import { motion, useReducedMotion } from 'motion/react';
import { cn } from '@/lib/utils';

export interface SegmentedOption<T extends string> {
  value: T;
  label: React.ReactNode;
  icon?: React.ReactNode;
  count?: number;
}

interface SegmentedProps<T extends string> {
  value: T;
  options: Array<SegmentedOption<T>>;
  onChange: (value: T) => void;
  layoutId?: string;
  className?: string;
  ariaLabel?: string;
}

export function Segmented<T extends string>({
  value,
  options,
  onChange,
  layoutId,
  className,
  ariaLabel,
}: SegmentedProps<T>) {
  const reduce = useReducedMotion();
  return (
    <div
      role="group"
      aria-label={ariaLabel}
      className={cn(
        'inline-flex h-8 items-center gap-0.5 rounded-md border border-border bg-muted/60 p-0.5',
        className,
      )}
    >
      {options.map((option) => {
        const active = option.value === value;
        return (
          <button
            key={option.value}
            type="button"
            aria-pressed={active}
            onClick={() => onChange(option.value)}
            className={cn(
              'relative inline-flex h-7 flex-1 items-center justify-center gap-1.5 whitespace-nowrap rounded-[5px] px-2.5 text-xs font-medium outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring/40 sm:flex-none',
              active ? 'text-foreground' : 'text-muted-foreground hover:text-foreground',
            )}
          >
            {active && layoutId && (
              <motion.span
                layoutId={layoutId}
                className="absolute inset-0 rounded-[5px] bg-card shadow-xs"
                transition={
                  reduce ? { duration: 0 } : { type: 'spring', stiffness: 500, damping: 38 }
                }
              />
            )}
            {active && !layoutId && (
              <span className="absolute inset-0 rounded-[5px] bg-card shadow-xs" />
            )}
            <span className="relative z-10 inline-flex items-center gap-1.5">
              {option.icon}
              {option.label}
              {typeof option.count === 'number' && (
                <span className="tabular-nums text-[10px] text-muted-foreground">
                  {option.count}
                </span>
              )}
            </span>
          </button>
        );
      })}
    </div>
  );
}
