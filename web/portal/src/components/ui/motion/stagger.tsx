import { motion, useReducedMotion, type HTMLMotionProps, type Variants } from 'motion/react';

export interface StaggerContainerProps extends HTMLMotionProps<'div'> {
  staggerDelay?: number;
  initialDelay?: number;
}

export function StaggerContainer({
  children,
  className,
  staggerDelay = 0.05,
  initialDelay = 0,
  ...props
}: StaggerContainerProps) {
  const reduce = useReducedMotion();

  const containerVariants: Variants = {
    hidden: { opacity: 0 },
    show: {
      opacity: 1,
      transition: {
        staggerChildren: reduce ? 0 : staggerDelay,
        delayChildren: reduce ? 0 : initialDelay,
      },
    },
  };

  return (
    <motion.div
      variants={containerVariants}
      initial="hidden"
      animate="show"
      className={className}
      {...props}
    >
      {children}
    </motion.div>
  );
}

export interface StaggerItemProps extends HTMLMotionProps<'div'> {
  yOffset?: number;
}

export function StaggerItem({
  children,
  className,
  yOffset = 8,
  ...props
}: StaggerItemProps) {
  const reduce = useReducedMotion();

  const itemVariants: Variants = {
    hidden: reduce ? { opacity: 0 } : { opacity: 0, y: yOffset },
    show: {
      opacity: 1,
      y: 0,
      transition: {
        duration: 0.24,
        ease: [0.16, 1, 0.3, 1] as const,
      },
    },
  };

  return (
    <motion.div variants={itemVariants} className={className} {...props}>
      {children}
    </motion.div>
  );
}
