import * as React from 'react';
import * as DialogPrimitive from '@radix-ui/react-dialog';
import { AnimatePresence, motion, useReducedMotion } from 'motion/react';
import { X } from 'lucide-react';
import { cn } from '@/lib/utils';

interface ModalProps {
  open: boolean;
  onOpenChange?: (open: boolean) => void;
  title: React.ReactNode;
  description?: React.ReactNode;
  children?: React.ReactNode;
  footer?: React.ReactNode;
  className?: string;
  hideClose?: boolean;
  preventClose?: boolean;
}

export function Modal({
  open,
  onOpenChange,
  title,
  description,
  children,
  footer,
  className,
  hideClose,
  preventClose,
}: ModalProps) {
  const reduce = useReducedMotion();
  const handleOpenChange = (next: boolean) => {
    if (!next && preventClose) return;
    onOpenChange?.(next);
  };

  return (
    <DialogPrimitive.Root open={open} onOpenChange={handleOpenChange}>
      <AnimatePresence>
        {open && (
          <DialogPrimitive.Portal forceMount>
            <DialogPrimitive.Overlay asChild forceMount>
              <motion.div
                className="fixed inset-0 z-50 bg-black/60 backdrop-blur-[3px]"
                initial={{ opacity: 0 }}
                animate={{ opacity: 1 }}
                exit={{ opacity: 0 }}
                transition={{ duration: 0.15 }}
              />
            </DialogPrimitive.Overlay>
            <DialogPrimitive.Content asChild forceMount>
              <motion.div
                className="fixed inset-0 z-50 grid place-items-center overflow-y-auto p-4 sm:p-6"
                initial={{ opacity: 0 }}
                animate={{ opacity: 1 }}
                exit={{ opacity: 0 }}
                transition={{ duration: 0.15 }}
                onPointerDown={(event) => {
                  if (event.target === event.currentTarget) handleOpenChange(false);
                }}
              >
                <motion.div
                  className={cn(
                    'pointer-events-auto relative w-full max-w-md rounded-xl border border-border bg-popover p-5 text-popover-foreground shadow-2xl outline-none',
                    className,
                  )}
                  initial={reduce ? false : { opacity: 0, y: 10, scale: 0.98 }}
                  animate={{ opacity: 1, y: 0, scale: 1 }}
                  exit={reduce ? { opacity: 0 } : { opacity: 0, y: 6, scale: 0.985 }}
                  transition={{ type: 'spring', stiffness: 420, damping: 32 }}
                >
                  <div className="flex flex-col gap-1.5 pr-8">
                    <DialogPrimitive.Title className="text-base font-semibold leading-none">
                      {title}
                    </DialogPrimitive.Title>
                    {description ? (
                      <DialogPrimitive.Description className="text-[13px] leading-relaxed text-muted-foreground">
                        {description}
                      </DialogPrimitive.Description>
                    ) : (
                      <DialogPrimitive.Description className="sr-only">对话框</DialogPrimitive.Description>
                    )}
                  </div>
                  {!hideClose && (
                    <DialogPrimitive.Close asChild>
                      <button
                        type="button"
                        aria-label="关闭"
                        className="absolute right-3.5 top-3.5 grid size-7 place-items-center rounded-md text-muted-foreground outline-none transition-colors hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/40"
                      >
                        <X className="size-4" />
                      </button>
                    </DialogPrimitive.Close>
                  )}
                  {children && <div className="mt-4">{children}</div>}
                  {footer && (
                    <div className="mt-5 flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
                      {footer}
                    </div>
                  )}
                </motion.div>
              </motion.div>
            </DialogPrimitive.Content>
          </DialogPrimitive.Portal>
        )}
      </AnimatePresence>
    </DialogPrimitive.Root>
  );
}
