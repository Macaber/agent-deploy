import * as React from 'react';
import { motion, useMotionTemplate, useMotionValue } from 'motion/react';
import { cn } from '@/lib/utils';

export interface SpotlightCardProps extends React.HTMLAttributes<HTMLDivElement> {
  spotlightColor?: string;
  spotlightSize?: number;
}

export function SpotlightCard({
  children,
  className,
  spotlightColor,
  spotlightSize = 300,
  ...props
}: SpotlightCardProps) {
  const mouseX = useMotionValue(-1000);
  const mouseY = useMotionValue(-1000);

  function handleMouseMove({ currentTarget, clientX, clientY }: React.MouseEvent<HTMLDivElement>) {
    const { left, top } = currentTarget.getBoundingClientRect();
    mouseX.set(clientX - left);
    mouseY.set(clientY - top);
  }

  function handleMouseLeave() {
    mouseX.set(-1000);
    mouseY.set(-1000);
  }

  const spotlightStyle = useMotionTemplate`radial-gradient(${spotlightSize}px circle at ${mouseX}px ${mouseY}px, ${
    spotlightColor || 'color-mix(in srgb, var(--primary) 10%, transparent)'
  }, transparent 75%)`;

  return (
    <div
      onMouseMove={handleMouseMove}
      onMouseLeave={handleMouseLeave}
      className={cn(
        'group relative overflow-hidden rounded-xl border border-border bg-card text-card-foreground shadow-xs transition-colors',
        className,
      )}
      {...props}
    >
      <motion.div
        className="pointer-events-none absolute -inset-px rounded-[inherit] opacity-0 transition-opacity duration-300 group-hover:opacity-100"
        style={{
          background: spotlightStyle,
        }}
        aria-hidden="true"
      />
      <div className="relative z-10 size-full">{children}</div>
    </div>
  );
}
