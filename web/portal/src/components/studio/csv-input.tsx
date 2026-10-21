import { useEffect, useState } from 'react';
import { Input } from '@/components/ui/input';

interface CsvInputProps {
  id?: string;
  value: string[];
  onChange: (value: string[]) => void;
  placeholder?: string;
  ariaLabel: string;
  className?: string;
}

/** Keeps the raw text locally so typing a comma does not fight the parsed array. */
export function CsvInput({ id, value, onChange, placeholder, ariaLabel, className }: CsvInputProps) {
  const [text, setText] = useState(value.join(', '));
  const [focused, setFocused] = useState(false);

  useEffect(() => {
    if (!focused) setText(value.join(', '));
  }, [value, focused]);

  return (
    <Input
      id={id}
      aria-label={ariaLabel}
      value={text}
      placeholder={placeholder}
      spellCheck={false}
      className={className}
      onFocus={() => setFocused(true)}
      onBlur={() => {
        setFocused(false);
        setText(value.join(', '));
      }}
      onChange={(event) => {
        const next = event.target.value;
        setText(next);
        onChange(
          next
            .split(',')
            .map((part) => part.trim())
            .filter(Boolean),
        );
      }}
    />
  );
}
