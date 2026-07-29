package com.example.workspace.model;

import com.fasterxml.jackson.annotation.JsonInclude;

@JsonInclude(JsonInclude.Include.NON_NULL)
public class StorageSpec {
    private String size;
    private String storageClass;

    public StorageSpec() {}

    public StorageSpec(String size, String storageClass) {
        this.size = size;
        this.storageClass = storageClass;
    }

    public String getSize() {
        return size;
    }

    public void setSize(String size) {
        this.size = size;
    }

    public String getStorageClass() {
        return storageClass;
    }

    public void setStorageClass(String storageClass) {
        this.storageClass = storageClass;
    }
}
