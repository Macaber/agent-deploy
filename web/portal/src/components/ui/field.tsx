import * as React from 'react';
import { cn } from '@/lib/utils';

interface FieldProps {
  label: string;
  htmlFor?: string;
  hint?: string;
  error?: string;
  required?: boolean;
  className?: string;
  children: React.ReactNode;
}

export function Field({ label, htmlFor, hint, error, required, className, children }: FieldProps) {
  return (
    <div className={cn('flex min-w-0 flex-col gap-1.5', className)}>
      <label htmlFor={htmlFor} className="flex items-center gap-1 text-xs font-medium text-foreground">
        {label}
        {required && <span className="text-destructive">*</span>}
      </label>
      {children}
      {error ? (
        <p className="text-[11px] leading-snug text-destructive" role="alert">
          {error}
        </p>
      ) : hint ? (
        <p className="text-[11px] leading-snug text-muted-foreground">{hint}</p>
      ) : null}
    </div>
  );
}
