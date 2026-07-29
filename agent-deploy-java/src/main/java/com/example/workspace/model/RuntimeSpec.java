package com.example.workspace.model;

import com.fasterxml.jackson.annotation.JsonInclude;
import java.util.List;

@JsonInclude(JsonInclude.Include.NON_NULL)
public class RuntimeSpec {
    private String image;
    private String cpu;
    private String memory;
    private List<EnvVar> env;
    private Integer port;
    private List<String> command;
    private List<String> args;
    private List<VolumeMount> volumeMounts;
    private String postStartScript;
    private String healthPath;
    private List<InitContainerSpec> initContainers;

    public RuntimeSpec() {}

    public String getImage() {
        return image;
    }

    public void setImage(String image) {
        this.image = image;
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

    public List<EnvVar> getEnv() {
        return env;
    }

    public void setEnv(List<EnvVar> env) {
        this.env = env;
    }

    public Integer getPort() {
        return port;
    }

    public void setPort(Integer port) {
        this.port = port;
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

    public List<InitContainerSpec> getInitContainers() {
        return initContainers;
    }

    public void setInitContainers(List<InitContainerSpec> initContainers) {
        this.initContainers = initContainers;
    }
}
