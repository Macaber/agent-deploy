package com.example.workspace.model;

import com.fasterxml.jackson.annotation.JsonInclude;

@JsonInclude(JsonInclude.Include.NON_NULL)
public class VolumeMount {
    private String mountPath;
    private String subPath;

    public VolumeMount() {}

    public VolumeMount(String mountPath, String subPath) {
        this.mountPath = mountPath;
        this.subPath = subPath;
    }

    public String getMountPath() {
        return mountPath;
    }

    public void setMountPath(String mountPath) {
        this.mountPath = mountPath;
    }

    public String getSubPath() {
        return subPath;
    }

    public void setSubPath(String subPath) {
        this.subPath = subPath;
    }
}
