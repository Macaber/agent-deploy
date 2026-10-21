import { clsx, type ClassValue } from 'clsx';
import { twMerge } from 'tailwind-merge';

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

let seq = 0;

/** crypto.randomUUID needs a secure context; the portal may be served over plain HTTP. */
export function uid(prefix = 'id'): string {
  seq += 1;
  return `${prefix}-${Date.now().toString(36)}-${seq.toString(36)}-${Math.random()
    .toString(36)
    .slice(2, 7)}`;
}

export function safeExternalUrl(url?: string): string | undefined {
  if (!url) return undefined;
  try {
    const parsed = new URL(url, window.location.origin);
    if (parsed.protocol === 'http:' || parsed.protocol === 'https:') return parsed.toString();
  } catch {
    /* ignore */
  }
  return undefined;
}

export function initials(value?: string): string {
  const text = (value || '?').trim();
  if (!text) return '?';
  const ascii = text.match(/[a-zA-Z0-9]/);
  return (ascii ? ascii[0] : text[0]).toUpperCase();
}

export function formatTime(value?: number): string {
  if (!value) return '—';
  return new Date(value).toLocaleTimeString('zh-CN', {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  });
}

export function sanitizeK8sName(value: string): string {
  const cleaned = value
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 63);
  return cleaned || 'workspace';
}

export function yamlString(value: string): string {
  return `"${String(value).replace(/\\/g, '\\\\').replace(/"/g, '\\"')}"`;
}

export async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
    /* fall through to the legacy path */
  }
  try {
    const area = document.createElement('textarea');
    area.value = text;
    area.setAttribute('readonly', '');
    area.style.position = 'fixed';
    area.style.opacity = '0';
    area.style.pointerEvents = 'none';
    document.body.appendChild(area);
    area.select();
    const ok = document.execCommand('copy');
    document.body.removeChild(area);
    return ok;
  } catch {
    return false;
  }
}
