package com.example.workspace.operator;

import com.example.workspace.model.ConfigMapVolumeMount;
import com.example.workspace.model.InitContainerSpec;
import com.example.workspace.model.SharedVolumeMount;
import com.example.workspace.model.Workspace;
import com.example.workspace.model.WorkspacePhase;
import com.example.workspace.model.WorkspaceStatus;

import io.fabric8.kubernetes.api.model.Container;
import io.fabric8.kubernetes.api.model.ContainerBuilder;
import io.fabric8.kubernetes.api.model.ContainerPort;
import io.fabric8.kubernetes.api.model.ContainerPortBuilder;
import io.fabric8.kubernetes.api.model.EnvVarBuilder;
import io.fabric8.kubernetes.api.model.IntOrString;
import io.fabric8.kubernetes.api.model.Lifecycle;
import io.fabric8.kubernetes.api.model.LifecycleBuilder;
import io.fabric8.kubernetes.api.model.PersistentVolumeClaim;
import io.fabric8.kubernetes.api.model.PersistentVolumeClaimBuilder;
import io.fabric8.kubernetes.api.model.PodList;
import io.fabric8.kubernetes.api.model.Probe;
import io.fabric8.kubernetes.api.model.ProbeBuilder;
import io.fabric8.kubernetes.api.model.Quantity;
import io.fabric8.kubernetes.api.model.ResourceRequirements;
import io.fabric8.kubernetes.api.model.ResourceRequirementsBuilder;
import io.fabric8.kubernetes.api.model.Service;
import io.fabric8.kubernetes.api.model.ServiceBuilder;
import io.fabric8.kubernetes.api.model.ServicePort;
import io.fabric8.kubernetes.api.model.ServicePortBuilder;
import io.fabric8.kubernetes.api.model.Volume;
import io.fabric8.kubernetes.api.model.VolumeBuilder;
import io.fabric8.kubernetes.api.model.VolumeMountBuilder;
import io.fabric8.kubernetes.api.model.apps.Deployment;
import io.fabric8.kubernetes.api.model.apps.DeploymentBuilder;
import io.fabric8.kubernetes.api.model.networking.v1.Ingress;
import io.fabric8.kubernetes.api.model.networking.v1.IngressBuilder;
import io.fabric8.kubernetes.client.KubernetesClient;
import io.javaoperatorsdk.operator.api.reconciler.Context;
import io.javaoperatorsdk.operator.api.reconciler.ControllerConfiguration;
import io.javaoperatorsdk.operator.api.reconciler.Reconciler;
import io.javaoperatorsdk.operator.api.reconciler.UpdateControl;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.stereotype.Component;

import java.io.File;
import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.Objects;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

@Component
@ControllerConfiguration
public class WorkspaceReconciler implements Reconciler<Workspace> {
    private static final Logger log = LoggerFactory.getLogger(WorkspaceReconciler.class);

    private final KubernetesClient client;

    public WorkspaceReconciler(KubernetesClient client) {
        this.client = client;
    }

    @Override
    public UpdateControl<Workspace> reconcile(Workspace ws, Context<Workspace> context) {
        String name = ws.getMetadata().getName();
        String namespace = ws.getMetadata().getNamespace();

        log.info("Reconciling Workspace {} in namespace {}", name, namespace);

        if (ws.getStatus() == null) {
            ws.setStatus(new WorkspaceStatus());
        }

        boolean statusNeedsUpdate = false;
        if (ws.getStatus().getPhase() == null) {
            ws.getStatus().setPhase(WorkspacePhase.PENDING);
            statusNeedsUpdate = true;
        }
        if (ws.getStatus().getLastActiveTime() == null) {
            ws.getStatus().setLastActiveTime(Instant.now());
            statusNeedsUpdate = true;
        }

        // 1. Reconcile PVC
        String pvcName = reconcilePVC(ws);

        // 2. Reconcile Deployment
        ReconcileDeployResult deployResult = reconcileDeployment(ws, pvcName);
        String deployName = deployResult.deployName;
        int reconciledReplicas = deployResult.reconciledReplicas;

        // 3. Reconcile Service
        String svcName = reconcileService(ws);

        // 4. Reconcile Ingress
        String endpoint = reconcileIngress(ws, svcName);

        // 5. Update Status & Idle Timeout Calculation
        WorkspacePhase oldPhase = ws.getStatus().getPhase();
        String oldPodName = ws.getStatus().getPodName();
        String oldPVCName = ws.getStatus().getPvcName();
        String oldEndpoint = ws.getStatus().getEndpoint();

        ws.getStatus().setPvcName(pvcName);
        ws.getStatus().setEndpoint(endpoint);

        Deployment deploy = client.apps().deployments().inNamespace(namespace).withName(deployName).get();
        if (deploy != null) {
            Boolean stopped = ws.getSpec().getStopped();
            if (Boolean.TRUE.equals(stopped)) {
                ws.getStatus().setPhase(WorkspacePhase.STOPPED);
                ws.getStatus().setPodName("");
            } else if (reconciledReplicas == 0) {
                ws.getStatus().setPhase(WorkspacePhase.SLEEPING);
                ws.getStatus().setPodName("");
            } else {
                Integer readyReplicas = deploy.getStatus() != null ? deploy.getStatus().getReadyReplicas() : 0;
                if (readyReplicas != null && readyReplicas > 0) {
                    ws.getStatus().setPhase(WorkspacePhase.RUNNING);

                    PodList pods = client.pods().inNamespace(namespace)
                            .withLabels(deploy.getSpec().getSelector().getMatchLabels())
                            .list();
                    if (pods != null && !pods.getItems().isEmpty()) {
                        ws.getStatus().setPodName(pods.getItems().get(0).getMetadata().getName());
                    }
                } else {
                    ws.getStatus().setPhase(WorkspacePhase.STARTING);
                    ws.getStatus().setPodName("");
                }
            }
        }

        // Transition logic for lastActiveTime
        if (ws.getStatus().getPhase() == WorkspacePhase.RUNNING) {
            if (oldPhase == WorkspacePhase.SLEEPING || oldPhase == WorkspacePhase.STOPPED ||
                oldPhase == WorkspacePhase.PENDING || oldPhase == WorkspacePhase.FAILED) {
                ws.getStatus().setLastActiveTime(Instant.now());
                statusNeedsUpdate = true;
            }
        }

        // Calculate next requeue for idle timeout
        Duration nextRequeue = null;
        if (ws.getStatus().getPhase() == WorkspacePhase.RUNNING && ws.getSpec().getIdleTimeout() != null && !ws.getSpec().getIdleTimeout().isBlank()) {
            try {
                Duration idleDuration = parseDuration(ws.getSpec().getIdleTimeout());
                Instant idleExpiry = ws.getStatus().getLastActiveTime().plus(idleDuration);
                Duration remainingIdle = Duration.between(Instant.now(), idleExpiry);
                if (!remainingIdle.isNegative() && !remainingIdle.isZero()) {
                    nextRequeue = remainingIdle;
                } else {
                    nextRequeue = Duration.ofSeconds(1);
                }
            } catch (Exception e) {
                log.error("Failed to parse idleTimeout {}", ws.getSpec().getIdleTimeout(), e);
            }
        }

        boolean changed = statusNeedsUpdate ||
                !Objects.equals(oldPhase, ws.getStatus().getPhase()) ||
                !Objects.equals(oldPodName, ws.getStatus().getPodName()) ||
                !Objects.equals(oldPVCName, ws.getStatus().getPvcName()) ||
                !Objects.equals(oldEndpoint, ws.getStatus().getEndpoint());

        if (changed) {
            log.info("Updating Workspace status: phase={}, podName={}", ws.getStatus().getPhase(), ws.getStatus().getPodName());
            if (nextRequeue != null) {
                return UpdateControl.<Workspace>patchStatus(ws).rescheduleAfter(nextRequeue);
            }
            return UpdateControl.<Workspace>patchStatus(ws);
        }

        if (nextRequeue != null) {
            return UpdateControl.<Workspace>noUpdate().rescheduleAfter(nextRequeue);
        }

        return UpdateControl.<Workspace>noUpdate();
    }

    private String reconcilePVC(Workspace ws) {
        String pvcName = ws.getMetadata().getName() + "-pvc";
        String namespace = ws.getMetadata().getNamespace();

        PersistentVolumeClaim existing = client.persistentVolumeClaims().inNamespace(namespace).withName(pvcName).get();
        if (existing == null) {
            String size = ws.getSpec().getStorage() != null && ws.getSpec().getStorage().getSize() != null
                    ? ws.getSpec().getStorage().getSize() : "10Gi";
            String storageClass = ws.getSpec().getStorage() != null ? ws.getSpec().getStorage().getStorageClass() : null;

            PersistentVolumeClaim pvc = new PersistentVolumeClaimBuilder()
                    .withNewMetadata()
                        .withName(pvcName)
                        .withNamespace(namespace)
                        .addNewOwnerReference()
                            .withApiVersion(ws.getApiVersion())
                            .withKind(ws.getKind())
                            .withName(ws.getMetadata().getName())
                            .withUid(ws.getMetadata().getUid())
                            .withController(true)
                            .withBlockOwnerDeletion(true)
                        .endOwnerReference()
                    .endMetadata()
                    .withNewSpec()
                        .withAccessModes("ReadWriteOnce")
                        .withStorageClassName(storageClass)
                        .withNewResources()
                            .addToRequests("storage", new Quantity(size))
                        .endResources()
                    .endSpec()
                    .build();

            client.persistentVolumeClaims().inNamespace(namespace).resource(pvc).create();
        }
        return pvcName;
    }

    private static class ReconcileDeployResult {
        String deployName;
        int reconciledReplicas;
        ReconcileDeployResult(String name, int replicas) {
            this.deployName = name;
            this.reconciledReplicas = replicas;
        }
    }

    private ReconcileDeployResult reconcileDeployment(Workspace ws, String pvcName) {
        String deployName = ws.getMetadata().getName() + "-deploy";
        String namespace = ws.getMetadata().getNamespace();

        int desiredReplicas = computeDesiredReplicas(ws);

        List<io.fabric8.kubernetes.api.model.EnvVar> envVars = new ArrayList<>();
        envVars.add(new EnvVarBuilder().withName("WORKSPACE_OWNER").withValue(ws.getSpec().getOwner()).build());
        envVars.add(new EnvVarBuilder().withName("WORKSPACE_NAME").withValue(ws.getMetadata().getName()).build());
        if (ws.getSpec().getRuntime() != null && ws.getSpec().getRuntime().getEnv() != null) {
            for (com.example.workspace.model.EnvVar env : ws.getSpec().getRuntime().getEnv()) {
                envVars.add(new EnvVarBuilder().withName(env.getName()).withValue(env.getValue()).build());
            }
        }

        // Resources
        String cpuStr = (ws.getSpec().getRuntime() != null && ws.getSpec().getRuntime().getCpu() != null)
                ? ws.getSpec().getRuntime().getCpu() : "500m";
        String memStr = (ws.getSpec().getRuntime() != null && ws.getSpec().getRuntime().getMemory() != null)
                ? ws.getSpec().getRuntime().getMemory() : "1Gi";

        ResourceRequirements resources = new ResourceRequirementsBuilder()
                .addToLimits("cpu", new Quantity(cpuStr))
                .addToLimits("memory", new Quantity(memStr))
                .addToRequests("cpu", new Quantity(cpuStr))
                .addToRequests("memory", new Quantity(memStr))
                .build();

        // Ports
        int containerPort = 8080;
        String image = ws.getSpec().getRuntime() != null ? ws.getSpec().getRuntime().getImage() : "";
        if (ws.getSpec().getRuntime() != null && ws.getSpec().getRuntime().getPort() != null && ws.getSpec().getRuntime().getPort() > 0) {
            containerPort = ws.getSpec().getRuntime().getPort();
        } else if (image.contains("nginx")) {
            containerPort = 80;
        }

        List<ContainerPort> ports = new ArrayList<>();
        ports.add(new ContainerPortBuilder().withName("http").withContainerPort(containerPort).build());
        if (Boolean.TRUE.equals(ws.getSpec().getExposeSSH())) {
            ports.add(new ContainerPortBuilder().withName("ssh").withContainerPort(22).build());
        }

        // HostPath vs PVC mode
        boolean useHostPath = "true".equalsIgnoreCase(System.getenv("USE_HOSTPATH")) ||
                System.getenv("WORKSPACE_DATA_DIR") != null ||
                System.getenv("KUBERNETES_SERVICE_HOST") == null;

        List<Volume> volumes = new ArrayList<>();
        if (useHostPath) {
            String dataDir = System.getenv("WORKSPACE_DATA_DIR");
            if (dataDir == null || dataDir.isBlank()) {
                dataDir = System.getProperty("user.dir") + "/data";
            }
            String hostPath = dataDir + "/" + ws.getMetadata().getName();

            for (String sub : Arrays.asList("workspace", "config", "share", "state", "cache")) {
                new File(hostPath + "/" + sub).mkdirs();
            }

            volumes.add(new VolumeBuilder()
                    .withName("workspace-data")
                    .withNewHostPath()
                        .withPath(hostPath)
                        .withType("DirectoryOrCreate")
                    .endHostPath()
                    .build());
        } else {
            volumes.add(new VolumeBuilder()
                    .withName("workspace-data")
                    .withNewPersistentVolumeClaim()
                        .withClaimName(pvcName)
                    .endPersistentVolumeClaim()
                    .build());
        }

        List<io.fabric8.kubernetes.api.model.VolumeMount> volumeMounts = new ArrayList<>();
        Map<String, String> sharedVolumeMap = new HashMap<>();

        if (ws.getSpec().getSharedVolumeMounts() != null) {
            for (SharedVolumeMount svm : ws.getSpec().getSharedVolumeMounts()) {
                String volName = sharedVolumeMap.computeIfAbsent(svm.getPvcName(), k -> "shared-vol-" + sharedVolumeMap.size());
                if (volumes.stream().noneMatch(v -> v.getName().equals(volName))) {
                    volumes.add(new VolumeBuilder()
                            .withName(volName)
                            .withNewPersistentVolumeClaim()
                                .withClaimName(svm.getPvcName())
                            .endPersistentVolumeClaim()
                            .build());
                }
                volumeMounts.add(new VolumeMountBuilder()
                        .withName(volName)
                        .withMountPath(svm.getMountPath())
                        .withSubPath(svm.getSubPath())
                        .withReadOnly(Boolean.TRUE.equals(svm.getReadOnly()))
                        .build());
            }
        }

        Map<String, String> configMapVolumeMap = new HashMap<>();
        if (ws.getSpec().getConfigMapVolumeMounts() != null) {
            for (ConfigMapVolumeMount cm : ws.getSpec().getConfigMapVolumeMounts()) {
                String volName = configMapVolumeMap.computeIfAbsent(cm.getConfigMapName(), k -> "cm-vol-" + configMapVolumeMap.size());
                if (volumes.stream().noneMatch(v -> v.getName().equals(volName))) {
                    volumes.add(new VolumeBuilder()
                            .withName(volName)
                            .withNewConfigMap()
                                .withName(cm.getConfigMapName())
                            .endConfigMap()
                            .build());
                }
                volumeMounts.add(new VolumeMountBuilder()
                        .withName(volName)
                        .withMountPath(cm.getMountPath())
                        .withSubPath(cm.getSubPath())
                        .withReadOnly(Boolean.TRUE.equals(cm.getReadOnly()))
                        .build());
            }
        }

        if (ws.getSpec().getRuntime() != null && ws.getSpec().getRuntime().getVolumeMounts() != null && !ws.getSpec().getRuntime().getVolumeMounts().isEmpty()) {
            for (com.example.workspace.model.VolumeMount vm : ws.getSpec().getRuntime().getVolumeMounts()) {
                volumeMounts.add(new VolumeMountBuilder()
                        .withName("workspace-data")
                        .withMountPath(vm.getMountPath())
                        .withSubPath(vm.getSubPath())
                        .build());
            }
        } else {
            volumeMounts.add(new VolumeMountBuilder()
                    .withName("workspace-data")
                    .withMountPath("/workspace")
                    .withSubPath("workspace")
                    .build());

            if (image.contains("smanx/opencode")) {
                volumeMounts.add(new VolumeMountBuilder().withName("workspace-data").withMountPath("/root/.config/opencode").withSubPath("config").build());
                volumeMounts.add(new VolumeMountBuilder().withName("workspace-data").withMountPath("/root/.local/share/opencode").withSubPath("share").build());
                volumeMounts.add(new VolumeMountBuilder().withName("workspace-data").withMountPath("/root/.local/state/opencode").withSubPath("state").build());
                volumeMounts.add(new VolumeMountBuilder().withName("workspace-data").withMountPath("/root/.cache/opencode").withSubPath("cache").build());
            }
        }

        // Init Containers
        List<Container> initContainers = new ArrayList<>();
        if (ws.getSpec().getRuntime() != null && ws.getSpec().getRuntime().getInitContainers() != null) {
            for (InitContainerSpec icSpec : ws.getSpec().getRuntime().getInitContainers()) {
                List<io.fabric8.kubernetes.api.model.EnvVar> icEnv = new ArrayList<>();
                if (icSpec.getEnv() != null) {
                    for (com.example.workspace.model.EnvVar e : icSpec.getEnv()) {
                        icEnv.add(new EnvVarBuilder().withName(e.getName()).withValue(e.getValue()).build());
                    }
                }
                List<io.fabric8.kubernetes.api.model.VolumeMount> icMounts = new ArrayList<>();
                if (icSpec.getVolumeMounts() != null) {
                    for (com.example.workspace.model.VolumeMount vm : icSpec.getVolumeMounts()) {
                        icMounts.add(new VolumeMountBuilder().withName("workspace-data").withMountPath(vm.getMountPath()).withSubPath(vm.getSubPath()).build());
                    }
                }
                initContainers.add(new ContainerBuilder()
                        .withName(icSpec.getName())
                        .withImage(icSpec.getImage())
                        .withImagePullPolicy("IfNotPresent")
                        .withCommand(icSpec.getCommand())
                        .withArgs(icSpec.getArgs())
                        .withEnv(icEnv)
                        .withVolumeMounts(icMounts)
                        .build());
            }
        }

        Map<String, String> labels = Map.of(
                "app", "workspace",
                "workspace", ws.getMetadata().getName()
        );

        List<String> command = null;
        List<String> args = null;
        if (ws.getSpec().getRuntime() != null) {
            if (ws.getSpec().getRuntime().getCommand() != null && !ws.getSpec().getRuntime().getCommand().isEmpty()) {
                command = ws.getSpec().getRuntime().getCommand();
            }
            if (ws.getSpec().getRuntime().getArgs() != null && !ws.getSpec().getRuntime().getArgs().isEmpty()) {
                args = ws.getSpec().getRuntime().getArgs();
            }
        }
        if (command == null && image.contains("smanx/opencode")) {
            command = List.of("/bin/bash", "-c");
            args = List.of("cd /workspace && exec /entrypoint.sh");
        }

        Lifecycle lifecycle = null;
        if (ws.getSpec().getRuntime() != null && ws.getSpec().getRuntime().getPostStartScript() != null && !ws.getSpec().getRuntime().getPostStartScript().isBlank()) {
            lifecycle = new LifecycleBuilder()
                    .withNewPostStart()
                        .withNewExec()
                            .withCommand("/bin/bash", "-c", ws.getSpec().getRuntime().getPostStartScript())
                        .endExec()
                    .endPostStart()
                    .build();
        }

        Probe readinessProbe = getReadinessProbe(ws, containerPort);

        Deployment existing = client.apps().deployments().inNamespace(namespace).withName(deployName).get();
        if (existing == null) {
            Deployment deploy = new DeploymentBuilder()
                    .withNewMetadata()
                        .withName(deployName)
                        .withNamespace(namespace)
                        .withLabels(labels)
                        .addNewOwnerReference()
                            .withApiVersion(ws.getApiVersion())
                            .withKind(ws.getKind())
                            .withName(ws.getMetadata().getName())
                            .withUid(ws.getMetadata().getUid())
                            .withController(true)
                            .withBlockOwnerDeletion(true)
                        .endOwnerReference()
                    .endMetadata()
                    .withNewSpec()
                        .withReplicas(desiredReplicas)
                        .withNewStrategy()
                            .withType("Recreate")
                        .endStrategy()
                        .withNewSelector()
                            .withMatchLabels(labels)
                        .endSelector()
                        .withNewTemplate()
                            .withNewMetadata()
                                .withLabels(labels)
                            .endMetadata()
                            .withNewSpec()
                                .withInitContainers(initContainers)
                                .withTerminationGracePeriodSeconds(2L)
                                .withAutomountServiceAccountToken(false)
                                .addNewContainer()
                                    .withName("runtime")
                                    .withImage(image)
                                    .withImagePullPolicy("IfNotPresent")
                                    .withCommand(command)
                                    .withArgs(args)
                                    .withPorts(ports)
                                    .withEnv(envVars)
                                    .withResources(resources)
                                    .withVolumeMounts(volumeMounts)
                                    .withLifecycle(lifecycle)
                                    .withReadinessProbe(readinessProbe)
                                .endContainer()
                                .withVolumes(volumes)
                            .endSpec()
                        .endTemplate()
                    .endSpec()
                    .build();

            client.apps().deployments().inNamespace(namespace).resource(deploy).create();
        } else {
            // Reconcile desired replicas if changed
            if (!Objects.equals(existing.getSpec().getReplicas(), desiredReplicas)) {
                client.apps().deployments().inNamespace(namespace).withName(deployName).scale(desiredReplicas);
            }
        }

        return new ReconcileDeployResult(deployName, desiredReplicas);
    }

    private int computeDesiredReplicas(Workspace ws) {
        if (Boolean.TRUE.equals(ws.getSpec().getStopped())) {
            return 0;
        }
        if (ws.getSpec().getIdleTimeout() != null && !ws.getSpec().getIdleTimeout().isBlank()
                && ws.getStatus() != null && ws.getStatus().getLastActiveTime() != null) {
            try {
                Duration idleDuration = parseDuration(ws.getSpec().getIdleTimeout());
                Instant idleExpiry = ws.getStatus().getLastActiveTime().plus(idleDuration);
                if (Instant.now().isAfter(idleExpiry)) {
                    log.info("Workspace {} idle timeout {} expired, scaling to 0", ws.getMetadata().getName(), ws.getSpec().getIdleTimeout());
                    return 0;
                }
            } catch (Exception e) {
                log.error("Failed to parse idleTimeout {}", ws.getSpec().getIdleTimeout(), e);
            }
        }
        return 1;
    }

    private String reconcileService(Workspace ws) {
        String svcName = ws.getMetadata().getName() + "-svc";
        String namespace = ws.getMetadata().getNamespace();

        int containerPort = 8080;
        String image = ws.getSpec().getRuntime() != null ? ws.getSpec().getRuntime().getImage() : "";
        if (ws.getSpec().getRuntime() != null && ws.getSpec().getRuntime().getPort() != null && ws.getSpec().getRuntime().getPort() > 0) {
            containerPort = ws.getSpec().getRuntime().getPort();
        } else if (image.contains("nginx")) {
            containerPort = 80;
        }

        Service existing = client.services().inNamespace(namespace).withName(svcName).get();
        if (existing == null) {
            List<ServicePort> ports = new ArrayList<>();
            ports.add(new ServicePortBuilder().withName("http").withPort(containerPort).withTargetPort(new IntOrString(containerPort)).build());
            if (Boolean.TRUE.equals(ws.getSpec().getExposeSSH())) {
                ports.add(new ServicePortBuilder().withName("ssh").withPort(22).withTargetPort(new IntOrString(22)).build());
            }

            Service svc = new ServiceBuilder()
                    .withNewMetadata()
                        .withName(svcName)
                        .withNamespace(namespace)
                        .addNewOwnerReference()
                            .withApiVersion(ws.getApiVersion())
                            .withKind(ws.getKind())
                            .withName(ws.getMetadata().getName())
                            .withUid(ws.getMetadata().getUid())
                            .withController(true)
                            .withBlockOwnerDeletion(true)
                        .endOwnerReference()
                    .endMetadata()
                    .withNewSpec()
                        .withType("ClusterIP")
                        .withSelector(Map.of("app", "workspace", "workspace", ws.getMetadata().getName()))
                        .withPorts(ports)
                    .endSpec()
                    .build();

            client.services().inNamespace(namespace).resource(svc).create();
        }
        return svcName;
    }

    private String reconcileIngress(Workspace ws, String svcName) {
        String ingressName = ws.getMetadata().getName() + "-ingress";
        String namespace = ws.getMetadata().getNamespace();

        int containerPort = 8080;
        String image = ws.getSpec().getRuntime() != null ? ws.getSpec().getRuntime().getImage() : "";
        if (ws.getSpec().getRuntime() != null && ws.getSpec().getRuntime().getPort() != null && ws.getSpec().getRuntime().getPort() > 0) {
            containerPort = ws.getSpec().getRuntime().getPort();
        } else if (image.contains("nginx")) {
            containerPort = 80;
        }

        String domain = System.getenv("INGRESS_DOMAIN");
        if (domain == null || domain.isBlank()) {
            domain = "workspace.example.com";
        }
        String host = ws.getMetadata().getName() + "." + domain;

        Ingress existing = client.network().v1().ingresses().inNamespace(namespace).withName(ingressName).get();
        if (existing == null) {
            Ingress ingress = new IngressBuilder()
                    .withNewMetadata()
                        .withName(ingressName)
                        .withNamespace(namespace)
                        .addToAnnotations("nginx.ingress.kubernetes.io/proxy-read-timeout", "3600")
                        .addToAnnotations("nginx.ingress.kubernetes.io/proxy-send-timeout", "3600")
                        .addNewOwnerReference()
                            .withApiVersion(ws.getApiVersion())
                            .withKind(ws.getKind())
                            .withName(ws.getMetadata().getName())
                            .withUid(ws.getMetadata().getUid())
                            .withController(true)
                            .withBlockOwnerDeletion(true)
                        .endOwnerReference()
                    .endMetadata()
                    .withNewSpec()
                        .withIngressClassName("nginx")
                        .addNewRule()
                            .withHost(host)
                            .withNewHttp()
                                .addNewPath()
                                    .withPath("/")
                                    .withPathType("Prefix")
                                    .withNewBackend()
                                        .withNewService()
                                            .withName(svcName)
                                            .withNewPort()
                                                .withNumber(containerPort)
                                            .endPort()
                                        .endService()
                                    .endBackend()
                                .endPath()
                            .endHttp()
                        .endRule()
                    .endSpec()
                    .build();

            client.network().v1().ingresses().inNamespace(namespace).resource(ingress).create();
        }
        return "http://" + host;
    }

    private Probe getReadinessProbe(Workspace ws, int containerPort) {
        String healthPath = ws.getSpec().getRuntime() != null ? ws.getSpec().getRuntime().getHealthPath() : null;
        if (healthPath != null && !healthPath.isBlank()) {
            return new ProbeBuilder()
                    .withNewHttpGet()
                        .withPath(healthPath)
                        .withPort(new IntOrString(containerPort))
                    .endHttpGet()
                    .withInitialDelaySeconds(5)
                    .withPeriodSeconds(5)
                    .withFailureThreshold(30)
                    .build();
        } else {
            return new ProbeBuilder()
                    .withNewTcpSocket()
                        .withPort(new IntOrString(containerPort))
                    .endTcpSocket()
                    .withInitialDelaySeconds(5)
                    .withPeriodSeconds(5)
                    .withFailureThreshold(30)
                    .build();
        }
    }

    private Duration parseDuration(String input) {
        if (input == null || input.isBlank()) {
            return Duration.ZERO;
        }
        input = input.trim();
        Pattern pattern = Pattern.compile("^(\\d+)([smhd])?$");
        Matcher matcher = pattern.matcher(input);
        if (matcher.matches()) {
            long val = Long.parseLong(matcher.group(1));
            String unit = matcher.group(2);
            if (unit == null || unit.equals("s")) {
                return Duration.ofSeconds(val);
            }
            switch (unit) {
                case "m": return Duration.ofMinutes(val);
                case "h": return Duration.ofHours(val);
                case "d": return Duration.ofDays(val);
            }
        }
        return Duration.ofSeconds(Long.parseLong(input));
    }
}
