import type { WorkspaceDetail, WorkspaceItem } from '@/types';

/** Resolve an API path relative to the path the portal is served from. */
export function apiUrl(path: string): string {
  const pathname = window.location.pathname;
  const segments = pathname.split('/');
  let basePath = '/';
  if (segments.length > 1) {
    const last = segments[segments.length - 1];
    if (last.includes('.') || last === '') {
      basePath = `${segments.slice(0, segments.length - 1).join('/')}/`;
    } else {
      basePath = `${segments.join('/')}/`;
    }
  }
  if (!basePath.startsWith('/')) basePath = `/${basePath}`;
  if (!basePath.endsWith('/')) basePath += '/';
  return basePath + path;
}

async function cleanError(response: Response, prefix: string): Promise<string> {
  try {
    const text = await response.text();
    if (/<html/i.test(text)) {
      const doc = new DOMParser().parseFromString(text, 'text/html');
      const heading = doc.querySelector('h1')?.textContent?.trim();
      if (heading) return `${prefix}: ${heading}`;
      const title = doc.querySelector('title')?.textContent?.trim();
      if (title) return `${prefix}: ${title}`;
      const body = doc.body?.textContent?.trim();
      if (body) return `${prefix}: ${body.slice(0, 150).replace(/\s+/g, ' ')}`;
    }
    return `${prefix}: ${text.trim() || response.statusText}`;
  } catch {
    return `${prefix}: ${response.statusText}`;
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(apiUrl(path), {
    credentials: 'same-origin',
    ...init,
    headers: {
      ...(init?.body ? { 'Content-Type': 'application/json' } : {}),
      ...(init?.headers || {}),
    },
  });
  if (!response.ok) {
    // A stale/expired session should surface the login dialog instead of
    // leaving the operator with a generic 401 banner.
    if (
      response.status === 401 &&
      !path.includes('api/login') &&
      !path.includes('api/auth/check')
    ) {
      window.dispatchEvent(new CustomEvent('bw:unauthorized'));
    }
    throw new Error(await cleanError(response, init?.method === 'POST' ? '请求失败' : '读取失败'));
  }
  return (await response.json()) as T;
}

export interface AuthStatus {
  authenticated: boolean;
  authEnabled: boolean;
}

export const api = {
  auth: () => request<AuthStatus>('api/auth/check'),
  login: (username: string, password: string) =>
    request<unknown>('api/login', {
      method: 'POST',
      body: JSON.stringify({ username, password }),
    }),
  logout: () => request<unknown>('api/logout', { method: 'POST' }),
  list: (namespace: string) =>
    request<WorkspaceItem[]>(`api/workspaces?namespace=${encodeURIComponent(namespace)}`),
  detail: (userId: string, namespace: string) =>
    request<WorkspaceDetail>(
      `api/workspaces?userId=${encodeURIComponent(userId)}&namespace=${encodeURIComponent(namespace)}`,
    ),
  create: (body: unknown) =>
    request<{ url?: string }>('api/workspaces', { method: 'POST', body: JSON.stringify(body) }),
  stop: (userId: string, namespace: string) =>
    request<unknown>('api/workspaces/stop', {
      method: 'POST',
      body: JSON.stringify({ userId, namespace }),
    }),
  wakeup: (userId: string, namespace: string) =>
    request<{ url?: string }>('api/workspaces/wakeup', {
      method: 'POST',
      body: JSON.stringify({ userId, namespace }),
    }),
};
