import { create } from 'zustand';
import type {
  ConfirmRequest,
  DirectoryFilter,
  InspectorTab,
  MainTab,
  Operation,
  Payload,
  Phase,
  Toast,
  ToastTone,
  WorkspaceItem,
} from '@/types';
import { api } from '@/lib/api';
import {
  createDefaultPayload,
  payloadFromSpec,
  toRequestPayload,
  validatePayload,
} from '@/lib/workspace';
import { uid } from '@/lib/utils';

type ListKey =
  | 'env'
  | 'volumeMounts'
  | 'sharedVolumeMounts'
  | 'configMapVolumeMounts'
  | 'initContainers';

type ListItem<K extends ListKey> = Payload[K][number];

interface FieldErrors {
  [key: string]: string;
}

interface DirectoryState {
  items: WorkspaceItem[];
  namespace: string;
  search: string;
  filter: DirectoryFilter;
  loading: boolean;
  error: string;
  lastUpdated: number;
  initialized: boolean;
}

interface TargetState {
  exists: boolean;
  phase: Phase | string;
  url: string;
  loading: boolean;
  checkedFor: string;
}

interface DeployResult {
  tone: 'success' | 'error';
  message: string;
  url?: string;
}

interface AppState {
  theme: 'dark' | 'light';
  tab: MainTab;
  inspectorTab: InspectorTab;
  payload: Payload;
  jsonDraft: string;
  jsonError: string | null;
  fieldErrors: FieldErrors;
  deployResult: DeployResult | null;

  directory: DirectoryState;
  target: TargetState;
  operation: Operation | null;

  auth: {
    authenticated: boolean;
    authEnabled: boolean;
    checking: boolean;
    loginOpen: boolean;
    loginSubmitting: boolean;
    loginError: string;
  };

  toasts: Toast[];
  confirm: ConfirmRequest | null;

  setTheme: (theme: 'dark' | 'light') => void;
  toggleTheme: () => void;

  setTab: (tab: MainTab) => void;
  setInspectorTab: (tab: InspectorTab) => void;

  updateField: <K extends keyof Payload>(key: K, value: Payload[K]) => void;
  patchPayload: (patch: Partial<Payload>) => void;
  setPayload: (payload: Payload) => void;
  addItem: <K extends ListKey>(key: K, item: ListItem<K>) => void;
  updateItem: <K extends ListKey>(key: K, id: string, patch: Partial<ListItem<K>>) => void;
  removeItem: (key: ListKey, id: string) => void;
  reorderItems: (key: ListKey, activeId: string, overId: string) => void;

  setJsonDraft: (text: string) => void;
  resetJsonDraft: () => void;

  setDirectoryNamespace: (namespace: string) => void;
  setDirectorySearch: (search: string) => void;
  setDirectoryFilter: (filter: DirectoryFilter) => void;
  fetchDirectory: (silent?: boolean) => Promise<void>;

  checkTarget: () => Promise<void>;
  deploy: () => Promise<boolean>;
  stopTarget: () => Promise<void>;
  wakeupTarget: () => Promise<void>;
  loadWorkspace: (userId: string, namespace: string) => Promise<void>;
  inlineStop: (userId: string, namespace: string) => Promise<void>;
  inlineWakeup: (userId: string, namespace: string) => Promise<void>;

  checkAuth: () => Promise<void>;
  login: (username: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
  closeLogin: () => void;

  pushToast: (tone: ToastTone, title: string, description?: string) => string;
  dismissToast: (id: string) => void;

  beginOperation: (title: string, detail?: string) => void;
  endOperation: () => void;

  askConfirm: (request: ConfirmRequest) => void;
  closeConfirm: () => void;
}

function readTheme(): 'dark' | 'light' {
  try {
    return localStorage.getItem('bw-theme') === 'light' ? 'light' : 'dark';
  } catch {
    return 'dark';
  }
}

function applyTheme(theme: 'dark' | 'light') {
  const dark = theme === 'dark';
  document.documentElement.classList.toggle('dark', dark);
  document.documentElement.style.colorScheme = dark ? 'dark' : 'light';
  document
    .querySelector('meta[name="theme-color"]')
    ?.setAttribute('content', dark ? '#09090b' : '#fafafa');
  try {
    localStorage.setItem('bw-theme', theme);
  } catch {
    /* ignore */
  }
}

function pretty(payload: Payload): string {
  return JSON.stringify(toRequestPayload(payload), null, 2);
}

function makeId(prefix: string, id?: unknown): string {
  return typeof id === 'string' && id ? id : uid(prefix);
}

function normalizeJsonPayload(raw: any, current: Payload): Payload {
  const record = raw && typeof raw === 'object' ? raw : {};
  return {
    ...current,
    ...record,
    userId: String(record.userId ?? current.userId),
    namespace: String(record.namespace ?? current.namespace),
    image: String(record.image ?? current.image),
    port: Number(record.port ?? current.port) || 0,
    cpu: String(record.cpu ?? current.cpu),
    memory: String(record.memory ?? current.memory),
    storageSize: String(record.storageSize ?? current.storageSize),
    storageClass: String(record.storageClass ?? current.storageClass),
    idleTimeout: String(record.idleTimeout ?? current.idleTimeout),
    healthPath: String(record.healthPath ?? current.healthPath),
    command: Array.isArray(record.command) ? record.command.map(String) : [],
    args: Array.isArray(record.args) ? record.args.map(String) : [],
    postStartScript: String(record.postStartScript ?? ''),
    env: Array.isArray(record.env)
      ? record.env.map((item: any) => ({
          id: makeId('env', item?.id),
          name: String(item?.name ?? ''),
          value: String(item?.value ?? ''),
        }))
      : [],
    volumeMounts: Array.isArray(record.volumeMounts)
      ? record.volumeMounts.map((item: any) => ({
          id: makeId('vol', item?.id),
          mountPath: String(item?.mountPath ?? ''),
          subPath: String(item?.subPath ?? ''),
        }))
      : [],
    sharedVolumeMounts: Array.isArray(record.sharedVolumeMounts)
      ? record.sharedVolumeMounts.map((item: any) => ({
          id: makeId('shared', item?.id),
          pvcName: String(item?.pvcName ?? ''),
          mountPath: String(item?.mountPath ?? ''),
          subPath: String(item?.subPath ?? ''),
          readOnly: !!item?.readOnly,
        }))
      : [],
    configMapVolumeMounts: Array.isArray(record.configMapVolumeMounts)
      ? record.configMapVolumeMounts.map((item: any) => ({
          id: makeId('cm', item?.id),
          configMapName: String(item?.configMapName ?? ''),
          mountPath: String(item?.mountPath ?? ''),
          subPath: String(item?.subPath ?? ''),
          readOnly: !!item?.readOnly,
        }))
      : [],
    initContainers: Array.isArray(record.initContainers)
      ? record.initContainers.map((item: any) => ({
          id: makeId('init', item?.id),
          name: String(item?.name ?? ''),
          image: String(item?.image ?? ''),
          command: Array.isArray(item?.command) ? item.command.map(String) : [],
          args: Array.isArray(item?.args) ? item.args.map(String) : [],
        }))
      : [],
  };
}

let targetSeq = 0;
let directorySeq = 0;

const initialPayload = createDefaultPayload();

export const useApp = create<AppState>((set, get) => ({
  theme: readTheme(),
  tab:
    new URLSearchParams(window.location.search).get('tab') === 'studio' ? 'studio' : 'directory',
  inspectorTab: 'json',
  payload: initialPayload,
  jsonDraft: pretty(initialPayload),
  jsonError: null,
  fieldErrors: {},
  deployResult: null,

  directory: {
    items: [],
    namespace: 'bocomwork',
    search: '',
    filter: 'all',
    loading: false,
    error: '',
    lastUpdated: 0,
    initialized: false,
  },

  target: {
    exists: false,
    phase: 'Unknown',
    url: '',
    loading: false,
    checkedFor: '',
  },

  operation: null,

  auth: {
    authenticated: true,
    authEnabled: false,
    checking: true,
    loginOpen: false,
    loginSubmitting: false,
    loginError: '',
  },

  toasts: [],
  confirm: null,

  setTheme: (theme) => {
    applyTheme(theme);
    set({ theme });
  },

  toggleTheme: () => {
    const next = get().theme === 'dark' ? 'light' : 'dark';
    applyTheme(next);
    set({ theme: next });
  },

  setTab: (tab) => {
    set({ tab });
    if (tab === 'studio') {
      get().checkTarget();
    } else {
      const { auth, directory } = get();
      if (auth.checking || !auth.authenticated || auth.loginOpen) return;
      if (!directory.initialized || Date.now() - directory.lastUpdated > 15_000) {
        void get().fetchDirectory(true);
      }
    }
  },

  setInspectorTab: (tab) => {
    const { inspectorTab: previous, jsonError } = get();
    // Re-sync the JSON view from the form when returning to it, but never
    // discard an invalid draft: it must stay visible until fixed or reset.
    if (tab === 'json' && previous !== 'json' && !jsonError) get().resetJsonDraft();
    set({ inspectorTab: tab });
  },

  updateField: (key, value) => {
    set((state) => {
      const payload = { ...state.payload, [key]: value };
      return {
        payload,
        jsonDraft: pretty(payload),
        jsonError: null,
        deployResult: null,
        fieldErrors: { ...state.fieldErrors, [key]: '' },
      };
    });
  },

  patchPayload: (patch) => {
    set((state) => {
      const payload = { ...state.payload, ...patch };
      return {
        payload,
        jsonDraft: pretty(payload),
        jsonError: null,
        deployResult: null,
        fieldErrors: {},
      };
    });
  },

  setPayload: (payload) => {
    set({
      payload,
      jsonDraft: pretty(payload),
      jsonError: null,
      deployResult: null,
      fieldErrors: {},
    });
  },

  addItem: (key, item) => {
    set((state) => {
      const list = [...(state.payload[key] as any[]), item];
      const payload = { ...state.payload, [key]: list } as Payload;
      return {
        payload,
        jsonDraft: pretty(payload),
        jsonError: null,
        deployResult: null,
        fieldErrors: {},
      };
    });
  },

  updateItem: (key, id, patch) => {
    set((state) => {
      const list = (state.payload[key] as any[]).map((item) =>
        item.id === id ? { ...item, ...patch } : item,
      );
      const payload = { ...state.payload, [key]: list } as Payload;
      return {
        payload,
        jsonDraft: pretty(payload),
        jsonError: null,
        deployResult: null,
        fieldErrors: {},
      };
    });
  },

  removeItem: (key, id) => {
    set((state) => {
      const list = (state.payload[key] as any[]).filter((item) => item.id !== id);
      const payload = { ...state.payload, [key]: list } as Payload;
      return {
        payload,
        jsonDraft: pretty(payload),
        jsonError: null,
        deployResult: null,
        fieldErrors: {},
      };
    });
  },

  reorderItems: (key, activeId, overId) => {
    if (!activeId || !overId || activeId === overId) return;
    set((state) => {
      const list = [...(state.payload[key] as any[])];
      const from = list.findIndex((item) => item.id === activeId);
      const to = list.findIndex((item) => item.id === overId);
      if (from < 0 || to < 0) return state;
      const [moved] = list.splice(from, 1);
      list.splice(to, 0, moved);
      const payload = { ...state.payload, [key]: list } as Payload;
      return {
        payload,
        jsonDraft: pretty(payload),
        jsonError: null,
        deployResult: null,
        fieldErrors: {},
      };
    });
  },

  setJsonDraft: (text) => {
    set({ jsonDraft: text });
    try {
      const parsed = JSON.parse(text);
      if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
        set({ jsonError: 'JSON 根节点必须是对象' });
        return;
      }
      set((state) => ({
        payload: normalizeJsonPayload(parsed, state.payload),
        jsonError: null,
        deployResult: null,
        fieldErrors: {},
      }));
    } catch (error) {
      set({ jsonError: error instanceof Error ? error.message : 'JSON 格式错误' });
    }
  },

  resetJsonDraft: () => {
    set((state) => ({ jsonDraft: pretty(state.payload), jsonError: null }));
  },

  setDirectoryNamespace: (namespace) => {
    set((state) => ({ directory: { ...state.directory, namespace } }));
  },

  setDirectorySearch: (search) => {
    set((state) => ({ directory: { ...state.directory, search } }));
  },

  setDirectoryFilter: (filter) => {
    set((state) => ({ directory: { ...state.directory, filter } }));
  },

  fetchDirectory: async (silent = false) => {
    const current = get().directory;
    const namespace = current.namespace.trim() || 'bocomwork';
    const seq = ++directorySeq;
    set((state) => ({ directory: { ...state.directory, loading: true, error: '' } }));
    try {
      const items = await api.list(namespace);
      if (seq !== directorySeq) return;
      set((state) => ({
        directory: {
          ...state.directory,
          items: Array.isArray(items) ? items : [],
          loading: false,
          error: '',
          lastUpdated: Date.now(),
          initialized: true,
        },
      }));
    } catch (error) {
      if (seq !== directorySeq) return;
      const message = error instanceof Error ? error.message : '获取工作空间列表失败';
      set((state) => ({
        directory: { ...state.directory, loading: false, error: message, initialized: true },
      }));
      if (!silent) get().pushToast('error', '列表加载失败', message);
    }
  },

  checkTarget: async () => {
    const { userId, namespace } = get().payload;
    const key = `${userId.trim()}|${namespace.trim()}`;
    const seq = ++targetSeq;
    if (!userId.trim()) {
      set({
        target: { exists: false, phase: 'Unknown', url: '', loading: false, checkedFor: key },
      });
      return;
    }
    set((state) => {
      const sameTarget = state.target.checkedFor === key;
      return {
        target: {
          ...state.target,
          ...(sameTarget ? {} : { exists: false, phase: 'Unknown', url: '' }),
          loading: true,
          checkedFor: key,
        },
      };
    });
    try {
      const detail = await api.detail(userId.trim(), namespace.trim() || 'bocomwork');
      if (seq !== targetSeq) return;
      set({
        target: {
          exists: !!detail.exists,
          phase: (detail.phase as Phase) || 'Unknown',
          url: detail.url || '',
          loading: false,
          checkedFor: key,
        },
      });
    } catch {
      if (seq !== targetSeq) return;
      set((state) => ({ target: { ...state.target, loading: false } }));
    }
  },

  deploy: async () => {
    const { payload } = get();
    const errors = validatePayload(payload);
    if (Object.keys(errors).length) {
      set({ fieldErrors: errors, deployResult: { tone: 'error', message: '请先修正表单中的错误' } });
      get().pushToast('error', '配置校验未通过', Object.values(errors)[0]);
      return false;
    }
    get().beginOperation('正在部署工作空间…', '提交定制规格并等待 K8s 就绪');
    set({ deployResult: null, fieldErrors: {} });
    try {
      const result = await api.create(toRequestPayload(payload));
      set({
        deployResult: {
          tone: 'success',
          message: `工作空间 ${payload.userId} 已就绪`,
          url: result.url,
        },
      });
      get().pushToast('success', '部署成功', `实例 ${payload.userId} 已就绪`);
      await Promise.all([get().checkTarget(), get().fetchDirectory(true)]);
      return true;
    } catch (error) {
      const message = error instanceof Error ? error.message : '部署失败';
      set({ deployResult: { tone: 'error', message } });
      get().pushToast('error', '部署失败', message);
      return false;
    } finally {
      get().endOperation();
    }
  },

  stopTarget: async () => {
    const { userId, namespace } = get().payload;
    if (!userId.trim()) return;
    get().beginOperation('正在停止工作空间…', '向集群下发停止指令');
    try {
      await api.stop(userId.trim(), namespace.trim() || 'bocomwork');
      get().pushToast('info', '工作空间已停止', userId);
      await Promise.all([get().checkTarget(), get().fetchDirectory(true)]);
    } catch (error) {
      get().pushToast('error', '停止失败', error instanceof Error ? error.message : '未知错误');
    } finally {
      get().endOperation();
    }
  },

  wakeupTarget: async () => {
    const { userId, namespace } = get().payload;
    if (!userId.trim()) return;
    get().beginOperation('正在唤醒工作空间…', '调度容器并建立访问入口');
    try {
      await api.wakeup(userId.trim(), namespace.trim() || 'bocomwork');
      get().pushToast('success', '工作空间已唤醒', userId);
      await Promise.all([get().checkTarget(), get().fetchDirectory(true)]);
    } catch (error) {
      get().pushToast('error', '唤醒失败', error instanceof Error ? error.message : '未知错误');
    } finally {
      get().endOperation();
    }
  },

  loadWorkspace: async (userId, namespace) => {
    get().beginOperation('正在载入工作空间配置…', '从集群读取完整的规格');
    try {
      const detail = await api.detail(userId, namespace);
      if (!detail.exists || !detail.spec) {
        get().pushToast('error', '载入失败', '该工作空间不存在或没有可用规格');
        return;
      }
      const base = createDefaultPayload();
      const payload: Payload = {
        ...base,
        ...payloadFromSpec(userId, namespace, detail.spec),
      } as Payload;
      set({
        payload,
        jsonDraft: pretty(payload),
        jsonError: null,
        tab: 'studio',
        deployResult: null,
        fieldErrors: {},
      });
      get().pushToast('success', '配置已载入', `用户 ${userId}`);
      void get().checkTarget();
    } catch (error) {
      get().pushToast('error', '载入失败', error instanceof Error ? error.message : '未知错误');
    } finally {
      get().endOperation();
    }
  },

  inlineStop: async (userId, namespace) => {
    get().beginOperation('正在停止工作空间…', userId);
    try {
      await api.stop(userId, namespace || 'bocomwork');
      get().pushToast('info', '工作空间已停止', userId);
      await get().fetchDirectory(true);
    } catch (error) {
      get().pushToast('error', '停止失败', error instanceof Error ? error.message : '未知错误');
    } finally {
      get().endOperation();
    }
  },

  inlineWakeup: async (userId, namespace) => {
    get().beginOperation('正在唤醒工作空间…', userId);
    try {
      await api.wakeup(userId, namespace || 'bocomwork');
      get().pushToast('success', '工作空间已唤醒', userId);
      await get().fetchDirectory(true);
    } catch (error) {
      get().pushToast('error', '唤醒失败', error instanceof Error ? error.message : '未知错误');
    } finally {
      get().endOperation();
    }
  },

  checkAuth: async () => {
    set((state) => ({ auth: { ...state.auth, checking: true } }));
    try {
      const status = await api.auth();
      set((state) => ({
        auth: {
          ...state.auth,
          authenticated: !!status.authenticated,
          authEnabled: !!status.authEnabled,
          checking: false,
          loginOpen: !!status.authEnabled && !status.authenticated,
          loginError: '',
        },
      }));
    } catch {
      // If auth status cannot be read, do not lock the operator out.
      set((state) => ({
        auth: { ...state.auth, checking: false, authenticated: true, authEnabled: false },
      }));
    }
  },

  login: async (username, password) => {
    const user = username.trim();
    if (!user || !password.trim()) {
      set((state) => ({ auth: { ...state.auth, loginError: '用户名与密码均为必填项' } }));
      return;
    }
    set((state) => ({
      auth: { ...state.auth, loginSubmitting: true, loginError: '' },
    }));
    try {
      await api.login(user, password);
      set((state) => ({
        auth: { ...state.auth, loginSubmitting: false, loginOpen: false, authenticated: true },
      }));
      get().pushToast('success', '登录成功', '欢迎回到工作空间控制台');
    } catch (error) {
      set((state) => ({
        auth: {
          ...state.auth,
          loginSubmitting: false,
          loginError: error instanceof Error ? error.message : '登录失败',
        },
      }));
    }
  },

  logout: async () => {
    try {
      await api.logout();
    } finally {
      set((state) => ({
        auth: {
          ...state.auth,
          authenticated: false,
          loginOpen: state.auth.authEnabled,
          loginError: '',
        },
        directory: { ...state.directory, items: [], initialized: false },
      }));
      get().pushToast('info', '已退出登录');
      set({ tab: 'directory' });
    }
  },

  closeLogin: () => {
    const auth = get().auth;
    if (auth.authEnabled && !auth.authenticated) return;
    set((state) => ({ auth: { ...state.auth, loginOpen: false, loginError: '' } }));
  },

  pushToast: (tone, title, description) => {
    const id = uid('toast');
    set((state) => ({
      toasts: [...state.toasts.slice(-3), { id, tone, title, description }],
    }));
    return id;
  },

  dismissToast: (id) => {
    set((state) => ({ toasts: state.toasts.filter((toast) => toast.id !== id) }));
  },

  beginOperation: (title, detail) => set({ operation: { title, detail } }),
  endOperation: () => set({ operation: null }),

  askConfirm: (request) => set({ confirm: request }),
  closeConfirm: () => set({ confirm: null }),
}));

// Keep document theme in sync on first load as well.
applyTheme(readTheme());
