package com.example.workspace.model;

import com.fasterxml.jackson.annotation.JsonInclude;
import java.util.List;

@JsonInclude(JsonInclude.Include.NON_NULL)
public class InitContainerSpec {
    private String name;
    private String image;
    private List<String> command;
    private List<String> args;
    private List<EnvVar> env;
    private List<VolumeMount> volumeMounts;
    private List<SharedVolumeMount> sharedVolumeMounts;
    private List<ConfigMapVolumeMount> configMapVolumeMounts;

    public InitContainerSpec() {}

    public String getName() {
        return name;
    }

    public void setName(String name) {
        this.name = name;
    }

    public String getImage() {
        return image;
    }

    public void setImage(String image) {
        this.image = image;
    }

    public List<String> getCommand() {
        return command;
    }

    public void setCommand(List<String> command) {
        this.command = command;
    }

    public List<String> getArgs() {
        return args;
    }

    public void setArgs(List<String> args) {
        this.args = args;
    }

    public List<EnvVar> getEnv() {
        return env;
    }

    public void setEnv(List<EnvVar> env) {
        this.env = env;
    }

    public List<VolumeMount> getVolumeMounts() {
        return volumeMounts;
    }

    public void setVolumeMounts(List<VolumeMount> volumeMounts) {
        this.volumeMounts = volumeMounts;
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
