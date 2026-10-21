export type Phase =
  | 'Pending'
  | 'Starting'
  | 'Running'
  | 'Sleeping'
  | 'Stopped'
  | 'Failed'
  | 'Unknown';

export interface WorkspaceItem {
  userId: string;
  name: string;
  namespace?: string;
  oa?: string;
  phase: Phase | string;
  url: string;
  image: string;
  cpu: string;
  memory: string;
}

export interface VolumeMount {
  id: string;
  mountPath: string;
  subPath: string;
}

export interface SharedVolumeMount {
  id: string;
  pvcName: string;
  mountPath: string;
  subPath: string;
  readOnly: boolean;
}

export interface ConfigMapVolumeMount {
  id: string;
  configMapName: string;
  mountPath: string;
  subPath: string;
  readOnly: boolean;
}

export interface EnvVar {
  id: string;
  name: string;
  value: string;
}

export interface InitContainer {
  id: string;
  name: string;
  image: string;
  command: string[];
  args: string[];
}

export interface Payload {
  userId: string;
  namespace: string;
  image: string;
  port: number;
  cpu: string;
  memory: string;
  storageSize: string;
  storageClass: string;
  idleTimeout: string;
  healthPath: string;
  command: string[];
  args: string[];
  postStartScript: string;
  env: EnvVar[];
  volumeMounts: VolumeMount[];
  sharedVolumeMounts: SharedVolumeMount[];
  configMapVolumeMounts: ConfigMapVolumeMount[];
  initContainers: InitContainer[];
}

export interface WorkspaceSpec {
  owner?: string;
  idleTimeout?: string;
  runtime?: {
    image?: string;
    port?: number;
    cpu?: string;
    memory?: string;
    healthPath?: string;
    command?: string[];
    args?: string[];
    postStartScript?: string;
    env?: Array<{ name: string; value: string }>;
    volumeMounts?: Array<{ mountPath: string; subPath?: string }>;
    initContainers?: Array<{
      name: string;
      image: string;
      command?: string[];
      args?: string[];
    }>;
  };
  storage?: { size?: string; storageClass?: string };
  sharedVolumeMounts?: Array<{
    pvcName: string;
    mountPath: string;
    subPath?: string;
    readOnly?: boolean;
  }>;
  configMapVolumeMounts?: Array<{
    configMapName: string;
    mountPath: string;
    subPath?: string;
    readOnly?: boolean;
  }>;
}

export interface WorkspaceDetail {
  exists: boolean;
  phase?: string;
  url?: string;
  oa?: string;
  spec?: WorkspaceSpec;
}

export type ToastTone = 'success' | 'error' | 'info';

export interface Toast {
  id: string;
  tone: ToastTone;
  title: string;
  description?: string;
}

export interface Operation {
  title: string;
  detail?: string;
}

export interface ConfirmRequest {
  title: string;
  description: string;
  confirmLabel?: string;
  tone?: 'default' | 'destructive';
  onConfirm: () => void | Promise<void>;
}

export type DirectoryFilter = 'all' | 'running' | 'idle' | 'failed';
export type MainTab = 'directory' | 'studio';
export type InspectorTab = 'json' | 'yaml' | 'curl';
