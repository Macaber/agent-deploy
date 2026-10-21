import type { Payload, WorkspaceSpec } from '@/types';
import { sanitizeK8sName, uid, yamlString } from '@/lib/utils';
import { apiUrl } from '@/lib/api';

export const ENV_TEMPLATE: Array<{ name: string; value: string }> = [
  {
    name: 'OPENCODE_RESOURCE_ATTRIBUTES',
    value:
      'service.name=opencode,workspace.owner=$(WORKSPACE_OWNER),workspace.name=$(WORKSPACE_NAME)',
  },
  { name: 'AIONUI_DATA_DIR', value: '/root/.bocomwork' },
  { name: 'OA', value: 'sun_yunfeng' },
  { name: 'GUWP_TOKEN', value: '' },
  { name: 'AIONUI_HOST', value: '0.0.0.0' },
  { name: 'WEBUI_PORT', value: '3000' },
  { name: 'AIONUI_ALLOW_REMOTE', value: '1' },
  { name: 'BOCOMWORK_EMAIL_IMAP_HOST', value: '' },
  { name: 'SKILLHUB_API_BASE', value: '' },
  { name: 'SKILLHUB_CHANNEL_RAND', value: '' },
  { name: 'SKILLHUB_PLATFORM', value: 'BOCOMCODE' },
  { name: 'OPENCODE_CONFIG', value: '/root/config/bocomcode.json' },
  { name: 'BOCOMWORK_EMAIL_SOCKS_PROXY', value: '' },
  { name: 'OPENCODE_PARSE_SERVICE_URL', value: '' },
  { name: 'OPENCODE_WORD_CONVERTER_URL', value: '' },
  { name: 'GUWP_USER_PROFILE_URL', value: '' },
];

export function createDefaultPayload(): Payload {
  return {
    userId: 'sun_yunfeng',
    namespace: 'bocomwork',
    image: 'docker.io/library/bocomwork:v1.0.2',
    port: 3000,
    cpu: '0.5',
    memory: '1Gi',
    storageSize: '',
    storageClass: '',
    idleTimeout: '10m',
    healthPath: '/health',
    command: ['bocomwork-entrypoint'],
    args: [],
    postStartScript: '',
    env: ENV_TEMPLATE.map((item) => ({ ...item, id: uid('env') })),
    volumeMounts: [
      { id: uid('vol'), mountPath: '/root/.bocomwork', subPath: 'bocomwork' },
      { id: uid('vol'), mountPath: '/root/.local/share/opencode', subPath: 'data' },
      { id: uid('vol'), mountPath: '/root/.config/opencode', subPath: 'data' },
    ],
    sharedVolumeMounts: [
      {
        id: uid('shared'),
        pvcName: 'bocomwork-local-share',
        mountPath: '/opt/bocom-defaults/skills/',
        subPath: 'skills',
        readOnly: false,
      },
      {
        id: uid('shared'),
        pvcName: 'bocomwork-local-share',
        mountPath: '/opt/bocom-defaults/dist/',
        subPath: 'dist',
        readOnly: false,
      },
      {
        id: uid('shared'),
        pvcName: 'bocomwork-local-share',
        mountPath: '/opt/bocom-defaults/opencodedir/',
        subPath: 'opencodedir',
        readOnly: false,
      },
    ],
    configMapVolumeMounts: [],
    initContainers: [],
  };
}

export function payloadFromSpec(
  userId: string,
  namespace: string,
  spec: WorkspaceSpec,
): Partial<Payload> {
  const runtime = spec.runtime || {};
  return {
    userId,
    namespace,
    image: runtime.image || '',
    port: runtime.port || 3000,
    cpu: runtime.cpu || '',
    memory: runtime.memory || '',
    storageSize: spec.storage?.size || '1Gi',
    storageClass: spec.storage?.storageClass || '',
    idleTimeout: spec.idleTimeout || '',
    healthPath: runtime.healthPath || '',
    command: runtime.command || [],
    args: runtime.args || [],
    postStartScript: runtime.postStartScript || '',
    env: (runtime.env || []).map((item) => ({ ...item, id: uid('env') })),
    volumeMounts: (runtime.volumeMounts || []).map((item) => ({
      id: uid('vol'),
      mountPath: item.mountPath,
      subPath: item.subPath || '',
    })),
    sharedVolumeMounts: (spec.sharedVolumeMounts || []).map((item) => ({
      id: uid('shared'),
      pvcName: item.pvcName,
      mountPath: item.mountPath,
      subPath: item.subPath || '',
      readOnly: !!item.readOnly,
    })),
    configMapVolumeMounts: (spec.configMapVolumeMounts || []).map((item) => ({
      id: uid('cm'),
      configMapName: item.configMapName,
      mountPath: item.mountPath,
      subPath: item.subPath || '',
      readOnly: !!item.readOnly,
    })),
    initContainers: (runtime.initContainers || []).map((item) => ({
      id: uid('init'),
      name: item.name,
      image: item.image,
      command: item.command || [],
      args: item.args || [],
    })),
  };
}

/** Strip client-only ids and blank rows before sending to the API. */
export function toRequestPayload(payload: Payload) {
  return {
    userId: payload.userId.trim(),
    namespace: payload.namespace.trim() || 'bocomwork',
    image: payload.image.trim(),
    port: payload.port,
    cpu: payload.cpu.trim(),
    memory: payload.memory.trim(),
    storageSize: payload.storageSize.trim(),
    storageClass: payload.storageClass.trim(),
    idleTimeout: payload.idleTimeout.trim(),
    healthPath: payload.healthPath.trim(),
    command: payload.command.filter(Boolean),
    args: payload.args.filter(Boolean),
    postStartScript: payload.postStartScript,
    env: payload.env
      .filter((item) => item.name.trim())
      .map(({ name, value }) => ({ name: name.trim(), value })),
    volumeMounts: payload.volumeMounts
      .filter((item) => item.mountPath.trim())
      .map(({ mountPath, subPath }) => ({ mountPath: mountPath.trim(), subPath: subPath.trim() })),
    sharedVolumeMounts: payload.sharedVolumeMounts
      .filter((item) => item.pvcName.trim() && item.mountPath.trim())
      .map(({ pvcName, mountPath, subPath, readOnly }) => ({
        pvcName: pvcName.trim(),
        mountPath: mountPath.trim(),
        subPath: subPath.trim(),
        readOnly,
      })),
    configMapVolumeMounts: payload.configMapVolumeMounts
      .filter((item) => item.configMapName.trim() && item.mountPath.trim())
      .map(({ configMapName, mountPath, subPath, readOnly }) => ({
        configMapName: configMapName.trim(),
        mountPath: mountPath.trim(),
        subPath: subPath.trim(),
        readOnly,
      })),
    initContainers: payload.initContainers
      .filter((item) => item.name.trim() && item.image.trim())
      .map(({ name, image, command, args }) => ({
        name: name.trim(),
        image: image.trim(),
        command: command.filter(Boolean),
        args: args.filter(Boolean),
      })),
  };
}

export type FieldErrors = Record<string, string>;

export function validatePayload(payload: Payload): FieldErrors {
  const errors: FieldErrors = {};
  if (!payload.userId.trim()) errors.userId = '请填写用户 ID';
  if (!payload.image.trim()) errors.image = '请填写容器镜像';
  if (!payload.port || payload.port < 1 || payload.port > 65535) {
    errors.port = '端口需在 1–65535 之间';
  }
  if (payload.idleTimeout && !/^(\d+(ms|s|m|h))+$/.test(payload.idleTimeout.trim())) {
    errors.idleTimeout = '格式示例：10m、2h';
  }
  if (payload.storageSize && !/^\d+(\.\d+)?(Ki|Mi|Gi|Ti|Pi|Ei)?$/.test(payload.storageSize.trim())) {
    errors.storageSize = '格式示例：10Gi';
  }
  payload.initContainers.forEach((item, index) => {
    if (!!item.name.trim() !== !!item.image.trim()) {
      errors[`init-${index}`] = 'Init 容器的名称与镜像需同时填写';
    }
  });
  return errors;
}

export function generateYaml(payload: Payload): string {
  const request = toRequestPayload(payload);
  const name = `ws-${sanitizeK8sName(request.userId || 'sample')}`;
  const lines: string[] = [
    'apiVersion: ai.example.com/v1alpha1',
    'kind: Workspace',
    'metadata:',
    `  name: ${name}`,
    `  namespace: ${request.namespace}`,
    'spec:',
    `  owner: ${yamlString(request.userId)}`,
  ];

  lines.push('  runtime:', `    image: ${yamlString(request.image)}`, `    port: ${request.port || 3000}`);
  if (request.cpu) lines.push(`    cpu: ${yamlString(request.cpu)}`);
  if (request.memory) lines.push(`    memory: ${yamlString(request.memory)}`);
  if (request.healthPath) lines.push(`    healthPath: ${yamlString(request.healthPath)}`);
  if (request.command.length) {
    lines.push('    command:', ...request.command.map((item) => `      - ${yamlString(item)}`));
  }
  if (request.args.length) {
    lines.push('    args:', ...request.args.map((item) => `      - ${yamlString(item)}`));
  }
  if (request.postStartScript) {
    lines.push(
      '    postStartScript: |',
      ...request.postStartScript.split('\n').map((line) => `      ${line}`),
    );
  }
  if (request.env.length) {
    lines.push('    env:');
    request.env.forEach((item) => {
      lines.push(`      - name: ${yamlString(item.name)}`, `        value: ${yamlString(item.value)}`);
    });
  }
  if (request.volumeMounts.length) {
    lines.push('    volumeMounts:');
    request.volumeMounts.forEach((item) => {
      lines.push(`      - mountPath: ${yamlString(item.mountPath)}`);
      if (item.subPath) lines.push(`        subPath: ${yamlString(item.subPath)}`);
    });
  }
  if (request.initContainers.length) {
    lines.push('    initContainers:');
    request.initContainers.forEach((item) => {
      lines.push(
        `      - name: ${yamlString(item.name)}`,
        `        image: ${yamlString(item.image)}`,
      );
      if (item.command.length) {
        lines.push('        command:', ...item.command.map((entry) => `          - ${yamlString(entry)}`));
      }
      if (item.args.length) {
        lines.push('        args:', ...item.args.map((entry) => `          - ${yamlString(entry)}`));
      }
    });
  }
  if (request.sharedVolumeMounts.length) {
    lines.push('  sharedVolumeMounts:');
    request.sharedVolumeMounts.forEach((item) => {
      lines.push(
        `    - pvcName: ${yamlString(item.pvcName)}`,
        `      mountPath: ${yamlString(item.mountPath)}`,
      );
      if (item.subPath) lines.push(`      subPath: ${yamlString(item.subPath)}`);
      if (item.readOnly) lines.push('      readOnly: true');
    });
  }
  if (request.configMapVolumeMounts.length) {
    lines.push('  configMapVolumeMounts:');
    request.configMapVolumeMounts.forEach((item) => {
      lines.push(
        `    - configMapName: ${yamlString(item.configMapName)}`,
        `      mountPath: ${yamlString(item.mountPath)}`,
      );
      if (item.subPath) lines.push(`      subPath: ${yamlString(item.subPath)}`);
      if (item.readOnly) lines.push('      readOnly: true');
    });
  }
  if (request.idleTimeout) lines.push(`  idleTimeout: ${yamlString(request.idleTimeout)}`);
  lines.push(
    '  storage:',
    `    size: ${yamlString(request.storageSize || '1Gi')}`,
  );
  if (request.storageClass) lines.push(`    storageClass: ${yamlString(request.storageClass)}`);
  return lines.join('\n');
}

export function generateCurl(payload: Payload): string {
  const endpoint = new URL(apiUrl('api/workspaces'), window.location.origin).toString();
  const body = JSON.stringify(toRequestPayload(payload)).replace(/'/g, `'\\''`);
  return `curl -X POST '${endpoint}' \\\n  -H 'Content-Type: application/json' \\\n  -d '${body}'`;
}
