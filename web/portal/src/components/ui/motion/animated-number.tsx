import { useEffect, useRef } from 'react';
import { useMotionValue, useSpring, useTransform, useReducedMotion, motion } from 'motion/react';

export interface AnimatedNumberProps {
  value: number;
  className?: string;
  springOptions?: {
    stiffness?: number;
    damping?: number;
    mass?: number;
  };
  formatter?: (val: number) => string;
}

export function AnimatedNumber({
  value,
  className,
  springOptions = { stiffness: 120, damping: 20 },
  formatter = (v) => Math.round(v).toLocaleString(),
}: AnimatedNumberProps) {
  const reduce = useReducedMotion();
  const motionVal = useMotionValue(value);
  const springVal = useSpring(motionVal, springOptions);
  const displayVal = useTransform(springVal, (current) => formatter(current));
  const spanRef = useRef<HTMLSpanElement>(null);

  useEffect(() => {
    motionVal.set(value);
  }, [value, motionVal]);

  useEffect(() => {
    return displayVal.on('change', (latest) => {
      if (spanRef.current) {
        spanRef.current.textContent = latest;
      }
    });
  }, [displayVal]);

  if (reduce) {
    return <span className={className}>{formatter(value)}</span>;
  }

  return (
    <motion.span ref={spanRef} className={className}>
      {formatter(value)}
    </motion.span>
  );
}
