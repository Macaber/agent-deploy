import * as React from 'react';
import { cva, type VariantProps } from 'class-variance-authority';
import { cn } from '@/lib/utils';

export const buttonVariants = cva(
  'inline-flex shrink-0 items-center justify-center gap-2 whitespace-nowrap rounded-md text-sm font-medium outline-none transition-[color,background-color,border-color,box-shadow,opacity] focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/40 disabled:pointer-events-none disabled:opacity-50 [&_svg]:pointer-events-none [&_svg]:shrink-0',
  {
    variants: {
      variant: {
        default:
          'border border-transparent bg-primary text-primary-foreground shadow-xs hover:bg-primary/90 active:bg-primary/85',
        secondary:
          'border border-transparent bg-secondary text-secondary-foreground hover:bg-secondary/80',
        outline:
          'border border-border bg-transparent text-foreground shadow-xs hover:bg-accent hover:text-accent-foreground',
        ghost: 'border border-transparent text-foreground hover:bg-accent hover:text-accent-foreground',
        destructive:
          'border border-transparent bg-destructive/10 text-destructive hover:bg-destructive/15 focus-visible:ring-destructive/30',
        success:
          'border border-transparent bg-success/10 text-success hover:bg-success/15 focus-visible:ring-success/30',
        link: 'border border-transparent text-foreground underline-offset-4 hover:underline',
      },
      size: {
        default: 'h-9 px-3.5',
        sm: 'h-8 gap-1.5 rounded-md px-2.5 text-xs',
        lg: 'h-10 rounded-md px-5',
        icon: 'size-9',
        iconSm: 'size-8 rounded-md',
      },
    },
    defaultVariants: {
      variant: 'default',
      size: 'default',
    },
  },
);

export interface ButtonProps
  extends React.ButtonHTMLAttributes<HTMLButtonElement>,
    VariantProps<typeof buttonVariants> {}

export const Button = React.forwardRef<HTMLButtonElement, ButtonProps>(
  ({ className, variant, size, type = 'button', ...props }, ref) => (
    <button
      ref={ref}
      type={type}
      className={cn(buttonVariants({ variant, size }), className)}
      {...props}
    />
  ),
);
Button.displayName = 'Button';
