package com.example.workspace.apiserver.service;

import com.example.workspace.apiserver.dto.WorkspaceItem;
import com.example.workspace.apiserver.dto.WorkspaceRequest;
import com.example.workspace.model.*;
import io.fabric8.kubernetes.api.model.ObjectMetaBuilder;
import io.fabric8.kubernetes.client.KubernetesClient;
import io.fabric8.kubernetes.client.dsl.MixedOperation;
import io.fabric8.kubernetes.client.dsl.Resource;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.stereotype.Service;

import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.time.Instant;
import java.util.*;
import java.util.stream.Collectors;

@Service
public class WorkspaceService {
    private static final Logger log = LoggerFactory.getLogger(WorkspaceService.class);

    private final KubernetesClient kubernetesClient;

    public WorkspaceService(KubernetesClient kubernetesClient) {
        this.kubernetesClient = kubernetesClient;
    }

    private MixedOperation<Workspace, io.fabric8.kubernetes.api.model.KubernetesResourceList<Workspace>, Resource<Workspace>> workspaceClient() {
        return kubernetesClient.resources(Workspace.class);
    }

    public static String buildWorkspaceName(String userId) {
        if (userId == null || userId.isBlank()) {
            return "ws-unknown";
        }
        try {
            MessageDigest digest = MessageDigest.getInstance("SHA-256");
            byte[] hash = digest.digest(userId.getBytes());
            StringBuilder hexString = new StringBuilder();
            for (byte b : hash) {
                String hex = Integer.toHexString(0xff & b);
                if (hex.length() == 1) hexString.append('0');
                hexString.append(hex);
            }
            String hashPrefix = hexString.substring(0, 8);
            String sanitized = userId.toLowerCase().replaceAll("[^a-z0-9]", "");
            if (sanitized.length() > 10) {
                sanitized = sanitized.substring(0, 10);
            }
            if (sanitized.isEmpty()) {
                sanitized = "user";
            }
            return "ws-" + sanitized + "-" + hashPrefix;
        } catch (NoSuchAlgorithmException e) {
            return "ws-" + userId.toLowerCase().replaceAll("[^a-z0-9]", "");
        }
    }

    public List<WorkspaceItem> listWorkspaces(String namespace) {
        if (namespace == null || namespace.isBlank()) {
            namespace = "default";
        }
        List<Workspace> list = workspaceClient().inNamespace(namespace).list().getItems();
        return list.stream().map(this::toItem).collect(Collectors.toList());
    }

    public WorkspaceItem getWorkspace(String userId, String namespace) {
        if (namespace == null || namespace.isBlank()) {
            namespace = "default";
        }
        String wsName = buildWorkspaceName(userId);
        Workspace ws = workspaceClient().inNamespace(namespace).withName(wsName).get();
        if (ws == null) {
            return null;
        }
        return toItem(ws);
    }

    public static String getEffectiveUserId(String userId, String namespace, List<EnvVar> envList) {
        if ("bocomwork".equals(namespace) && envList != null) {
            for (EnvVar env : envList) {
                if ("USER_CODE".equalsIgnoreCase(env.getName()) && env.getValue() != null && !env.getValue().isBlank()) {
                    return env.getValue().trim();
                }
            }
        }
        return userId;
    }

    public WorkspaceItem createOrUpdateWorkspace(WorkspaceRequest req) {
        String namespace = req.getNamespace() != null && !req.getNamespace().isBlank() ? req.getNamespace() : "default";
        String effectiveUserId = getEffectiveUserId(req.getUserId(), namespace, req.getEnv());
        String wsName = buildWorkspaceName(effectiveUserId);

        Workspace existing = workspaceClient().inNamespace(namespace).withName(wsName).get();

        String image = req.getImage() != null && !req.getImage().isBlank() ? req.getImage() : "smanx/opencode:v1";
        String cpu = req.getCpu() != null && !req.getCpu().isBlank() ? req.getCpu() : "1";
        String memory = req.getMemory() != null && !req.getMemory().isBlank() ? req.getMemory() : "2Gi";
        String storageSize = req.getStorageSize() != null && !req.getStorageSize().isBlank() ? req.getStorageSize() : "10Gi";
        String storageClass = req.getStorageClass();

        List<String> command = req.getCommand();
        if (command == null || command.isEmpty()) {
            command = req.getCmd();
        }

        RuntimeSpec runtimeSpec = new RuntimeSpec();
        runtimeSpec.setImage(image);
        runtimeSpec.setCpu(cpu);
        runtimeSpec.setMemory(memory);
        runtimeSpec.setPort(req.getPort());
        runtimeSpec.setEnv(req.getEnv());
        runtimeSpec.setCommand(command);
        runtimeSpec.setArgs(req.getArgs());
        runtimeSpec.setVolumeMounts(req.getVolumeMounts());
        runtimeSpec.setPostStartScript(req.getPostStartScript());
        runtimeSpec.setHealthPath(req.getHealthPath());
        runtimeSpec.setInitContainers(req.getInitContainers());

        StorageSpec storageSpec = new StorageSpec(storageSize, storageClass);

        WorkspaceSpec spec = new WorkspaceSpec();
        spec.setOwner(effectiveUserId);
        spec.setRuntime(runtimeSpec);
        spec.setStorage(storageSpec);
        spec.setExposeSSH(req.getExposeSSH());
        spec.setIdleTimeout(req.getIdleTimeout());
        spec.setStopped(false);
        spec.setSharedVolumeMounts(req.getSharedVolumeMounts());
        spec.setConfigMapVolumeMounts(req.getConfigMapVolumeMounts());

        if (existing == null) {
            Workspace ws = new Workspace();
            ws.setMetadata(new ObjectMetaBuilder()
                    .withName(wsName)
                    .withNamespace(namespace)
                    .build());
            ws.setSpec(spec);
            WorkspaceStatus status = new WorkspaceStatus();
            status.setPhase(WorkspacePhase.PENDING);
            status.setLastActiveTime(Instant.now());
            ws.setStatus(status);

            Workspace created = workspaceClient().inNamespace(namespace).resource(ws).create();
            return toItem(created);
        } else {
            existing.setSpec(spec);
            if (existing.getStatus() != null) {
                existing.getStatus().setLastActiveTime(Instant.now());
            }
            Workspace updated = workspaceClient().inNamespace(namespace).resource(existing).update();
            return toItem(updated);
        }
    }

    public WorkspaceItem stopWorkspace(String userId, String namespace) {
        if (namespace == null || namespace.isBlank()) {
            namespace = "default";
        }
        String wsName = buildWorkspaceName(userId);
        Workspace ws = workspaceClient().inNamespace(namespace).withName(wsName).get();
        if (ws == null) {
            return null;
        }
        ws.getSpec().setStopped(true);
        Workspace updated = workspaceClient().inNamespace(namespace).resource(ws).update();
        return toItem(updated);
    }

    public WorkspaceItem wakeupWorkspace(String userId, String namespace) {
        if (namespace == null || namespace.isBlank()) {
            namespace = "default";
        }
        String wsName = buildWorkspaceName(userId);
        Workspace ws = workspaceClient().inNamespace(namespace).withName(wsName).get();
        if (ws == null) {
            return null;
        }
        ws.getSpec().setStopped(false);
        if (ws.getStatus() != null) {
            ws.getStatus().setLastActiveTime(Instant.now());
        }
        Workspace updated = workspaceClient().inNamespace(namespace).resource(ws).update();
        return toItem(updated);
    }

    private WorkspaceItem toItem(Workspace ws) {
        String userId = ws.getSpec() != null ? ws.getSpec().getOwner() : "";
        String name = ws.getMetadata() != null ? ws.getMetadata().getName() : "";
        String phase = (ws.getStatus() != null && ws.getStatus().getPhase() != null)
                ? ws.getStatus().getPhase().getValue() : "Pending";
        String url = (ws.getStatus() != null && ws.getStatus().getEndpoint() != null)
                ? ws.getStatus().getEndpoint() : "";

        String image = (ws.getSpec() != null && ws.getSpec().getRuntime() != null)
                ? ws.getSpec().getRuntime().getImage() : "";
        String cpu = (ws.getSpec() != null && ws.getSpec().getRuntime() != null)
                ? ws.getSpec().getRuntime().getCpu() : "";
        String memory = (ws.getSpec() != null && ws.getSpec().getRuntime() != null)
                ? ws.getSpec().getRuntime().getMemory() : "";

        return new WorkspaceItem(userId, name, phase, url, image, cpu, memory);
    }
}
