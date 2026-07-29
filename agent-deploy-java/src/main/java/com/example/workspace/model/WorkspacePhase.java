package com.example.workspace.model;

import com.fasterxml.jackson.annotation.JsonValue;

public enum WorkspacePhase {
    PENDING("Pending"),
    STARTING("Starting"),
    RUNNING("Running"),
    SLEEPING("Sleeping"),
    STOPPED("Stopped"),
    FAILED("Failed");

    private final String value;

    WorkspacePhase(String value) {
        this.value = value;
    }

    @JsonValue
    public String getValue() {
        return value;
    }

    @Override
    public String toString() {
        return value;
    }
}
