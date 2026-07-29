package com.example.workspace.apiserver.dto;

import com.example.workspace.model.*;
import com.fasterxml.jackson.annotation.JsonInclude;
import java.util.List;

@JsonInclude(JsonInclude.Include.NON_NULL)
public class WorkspaceRequest {
    private String userId;
    private String namespace;
    private String image;
    private Integer port;
    private String cpu;
    private String memory;
    private String storageSize;
    private String storageClass;
    private String idleTimeout;
    private Boolean exposeSSH;
    private List<EnvVar> env;
    private List<String> command;
    private List<String> cmd;
    private List<String> args;
    private List<VolumeMount> volumeMounts;
    private String postStartScript;
    private String healthPath;
    private List<SharedVolumeMount> sharedVolumeMounts;
    private List<ConfigMapVolumeMount> configMapVolumeMounts;
    private List<InitContainerSpec> initContainers;

    public WorkspaceRequest() {}

    public String getUserId() {
        return userId;
    }

    public void setUserId(String userId) {
        this.userId = userId;
    }

    public String getNamespace() {
        return namespace;
    }

    public void setNamespace(String namespace) {
        this.namespace = namespace;
    }

    public String getImage() {
        return image;
    }

    public void setImage(String image) {
        this.image = image;
    }

    public Integer getPort() {
        return port;
    }

    public void setPort(Integer port) {
        this.port = port;
    }

    public String getCpu() {
        return cpu;
    }

    public void setCpu(String cpu) {
        this.cpu = cpu;
    }

    public String getMemory() {
        return memory;
    }

    public void setMemory(String memory) {
        this.memory = memory;
    }

    public String getStorageSize() {
        return storageSize;
    }

    public void setStorageSize(String storageSize) {
        this.storageSize = storageSize;
    }

    public String getStorageClass() {
        return storageClass;
    }

    public void setStorageClass(String storageClass) {
        this.storageClass = storageClass;
    }

    public String getIdleTimeout() {
        return idleTimeout;
    }

    public void setIdleTimeout(String idleTimeout) {
        this.idleTimeout = idleTimeout;
    }

    public Boolean getExposeSSH() {
        return exposeSSH;
    }

    public void setExposeSSH(Boolean exposeSSH) {
        this.exposeSSH = exposeSSH;
    }

    public List<EnvVar> getEnv() {
        return env;
    }

    public void setEnv(List<EnvVar> env) {
        this.env = env;
    }

    public List<String> getCommand() {
        return command;
    }

    public void setCommand(List<String> command) {
        this.command = command;
    }

    public List<String> getCmd() {
        return cmd;
    }

    public void setCmd(List<String> cmd) {
        this.cmd = cmd;
    }

    public List<String> getArgs() {
        return args;
    }

    public void setArgs(List<String> args) {
        this.args = args;
    }

    public List<VolumeMount> getVolumeMounts() {
        return volumeMounts;
    }

    public void setVolumeMounts(List<VolumeMount> volumeMounts) {
        this.volumeMounts = volumeMounts;
    }

    public String getPostStartScript() {
        return postStartScript;
    }

    public void setPostStartScript(String postStartScript) {
        this.postStartScript = postStartScript;
    }

    public String getHealthPath() {
        return healthPath;
    }

    public void setHealthPath(String healthPath) {
        this.healthPath = healthPath;
    }

    public List<SharedVolumeMount> getSharedVolumeMounts() {
        return sharedVolumeMounts;
    }

    public void setSharedVolumeMounts(List<SharedVolumeMount> sharedVolumeMounts) {
        this.sharedVolumeMounts = sharedVolumeMounts;
    }

    public List<ConfigMapVolumeMount> getConfigMapVolumeMounts() {
        return configMapVolumeMounts;
    }

    public void setConfigMapVolumeMounts(List<ConfigMapVolumeMount> configMapVolumeMounts) {
        this.configMapVolumeMounts = configMapVolumeMounts;
    }

    public List<InitContainerSpec> getInitContainers() {
        return initContainers;
    }

    public void setInitContainers(List<InitContainerSpec> initContainers) {
        this.initContainers = initContainers;
    }
}
