package com.example.workspace.model;

import io.fabric8.kubernetes.api.model.Namespaced;
import io.fabric8.kubernetes.client.CustomResource;
import io.fabric8.kubernetes.model.annotation.Group;
import io.fabric8.kubernetes.model.annotation.Kind;
import io.fabric8.kubernetes.model.annotation.Plural;
import io.fabric8.kubernetes.model.annotation.ShortNames;
import io.fabric8.kubernetes.model.annotation.Version;

@Group("ai.example.com")
@Version("v1alpha1")
@Kind("Workspace")
@Plural("workspaces")
@ShortNames("ws")
public class Workspace extends CustomResource<WorkspaceSpec, WorkspaceStatus> implements Namespaced {
    @Override
    protected WorkspaceStatus initStatus() {
        return new WorkspaceStatus();
    }
}
