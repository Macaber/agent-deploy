package com.example.workspace.apiserver.dto;

import com.fasterxml.jackson.annotation.JsonInclude;

@JsonInclude(JsonInclude.Include.NON_NULL)
public class WorkspaceItem {
    private String userId;
    private String name;
    private String phase;
    private String url;
    private String image;
    private String cpu;
    private String memory;

    public WorkspaceItem() {}

    public WorkspaceItem(String userId, String name, String phase, String url, String image, String cpu, String memory) {
        this.userId = userId;
        this.name = name;
        this.phase = phase;
        this.url = url;
        this.image = image;
        this.cpu = cpu;
        this.memory = memory;
    }

    public String getUserId() {
        return userId;
    }

    public void setUserId(String userId) {
        this.userId = userId;
    }

    public String getName() {
        return name;
    }

    public void setName(String name) {
        this.name = name;
    }

    public String getPhase() {
        return phase;
    }

    public void setPhase(String phase) {
        this.phase = phase;
    }

    public String getUrl() {
        return url;
    }

    public void setUrl(String url) {
        this.url = url;
    }

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
}
