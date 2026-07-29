package com.example.workspace.model;

import com.fasterxml.jackson.annotation.JsonInclude;
import java.util.List;

@JsonInclude(JsonInclude.Include.NON_NULL)
public class WorkspaceSpec {
    private String owner;
    private RepositorySpec repository;
    private RuntimeSpec runtime;
    private StorageSpec storage;
    private Boolean exposeSSH;
    private String idleTimeout;
    private Boolean stopped;
    private List<SharedVolumeMount> sharedVolumeMounts;
    private List<ConfigMapVolumeMount> configMapVolumeMounts;

    public WorkspaceSpec() {}

    public String getOwner() {
        return owner;
    }

    public void setOwner(String owner) {
        this.owner = owner;
    }

    public RepositorySpec getRepository() {
        return repository;
    }

    public void setRepository(RepositorySpec repository) {
        this.repository = repository;
    }

    public RuntimeSpec getRuntime() {
        return runtime;
    }

    public void setRuntime(RuntimeSpec runtime) {
        this.runtime = runtime;
    }

    public StorageSpec getStorage() {
        return storage;
    }

    public void setStorage(StorageSpec storage) {
        this.storage = storage;
    }

    public Boolean getExposeSSH() {
        return exposeSSH;
    }

    public void setExposeSSH(Boolean exposeSSH) {
        this.exposeSSH = exposeSSH;
    }

    public String getIdleTimeout() {
        return idleTimeout;
    }

    public void setIdleTimeout(String idleTimeout) {
        this.idleTimeout = idleTimeout;
    }

    public Boolean getStopped() {
        return stopped;
    }

    public void setStopped(Boolean stopped) {
        this.stopped = stopped;
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
}
