import * as React from 'react';
import * as TooltipPrimitive from '@radix-ui/react-tooltip';
import { motion } from 'motion/react';
import { cn } from '@/lib/utils';

export const TooltipProvider = TooltipPrimitive.Provider;

interface TipProps {
  label: React.ReactNode;
  children: React.ReactElement;
  side?: 'top' | 'right' | 'bottom' | 'left';
  align?: 'start' | 'center' | 'end';
  className?: string;
}

export function Tip({ label, children, side = 'top', align = 'center', className }: TipProps) {
  return (
    <TooltipPrimitive.Root>
      <TooltipPrimitive.Trigger asChild>{children}</TooltipPrimitive.Trigger>
      <TooltipPrimitive.Portal>
        <TooltipPrimitive.Content side={side} align={align} sideOffset={6} asChild>
          <motion.div
            initial={{ opacity: 0, scale: 0.97 }}
            animate={{ opacity: 1, scale: 1 }}
            transition={{ duration: 0.12 }}
            className={cn(
              'z-[70] max-w-64 rounded-md border border-border bg-popover px-2.5 py-1.5 text-xs font-medium text-popover-foreground shadow-lg',
              className,
            )}
          >
            {label}
          </motion.div>
        </TooltipPrimitive.Content>
      </TooltipPrimitive.Portal>
    </TooltipPrimitive.Root>
  );
}
