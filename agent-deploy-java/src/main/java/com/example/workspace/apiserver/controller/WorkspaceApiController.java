package com.example.workspace.apiserver.controller;

import com.example.workspace.apiserver.dto.WorkspaceItem;
import com.example.workspace.apiserver.dto.WorkspaceRequest;
import com.example.workspace.apiserver.service.WorkspaceService;
import org.springframework.boot.autoconfigure.condition.ConditionalOnProperty;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.*;

import java.util.List;
import java.util.Map;

@RestController
@RequestMapping("/api/workspaces")
@ConditionalOnProperty(name = "app.apiserver.enabled", havingValue = "true", matchIfMissing = true)
public class WorkspaceApiController {

    private final WorkspaceService workspaceService;

    public WorkspaceApiController(WorkspaceService workspaceService) {
        this.workspaceService = workspaceService;
    }

    @GetMapping
    public ResponseEntity<?> getOrListWorkspaces(@RequestParam(value = "userId", required = false) String userId,
                                                 @RequestParam(value = "namespace", required = false, defaultValue = "default") String namespace) {
        if (userId == null || userId.isBlank()) {
            List<WorkspaceItem> list = workspaceService.listWorkspaces(namespace);
            return ResponseEntity.ok(list);
        } else {
            WorkspaceItem item = workspaceService.getWorkspace(userId, namespace);
            if (item == null) {
                return ResponseEntity.status(HttpStatus.NOT_FOUND).body(Map.of("error", "workspace not found"));
            }
            return ResponseEntity.ok(item);
        }
    }

    @PostMapping
    public ResponseEntity<?> createOrUpdateWorkspace(@RequestBody WorkspaceRequest req) {
        if (req.getUserId() == null || req.getUserId().isBlank()) {
            return ResponseEntity.badRequest().body(Map.of("error", "userId is required"));
        }
        WorkspaceItem item = workspaceService.createOrUpdateWorkspace(req);
        return ResponseEntity.ok(item);
    }

    @PostMapping("/stop")
    public ResponseEntity<?> stopWorkspace(@RequestBody Map<String, String> body,
                                           @RequestParam(value = "namespace", required = false, defaultValue = "default") String namespace) {
        String userId = body.get("userId");
        if (userId == null || userId.isBlank()) {
            return ResponseEntity.badRequest().body(Map.of("error", "userId is required"));
        }
        WorkspaceItem item = workspaceService.stopWorkspace(userId, namespace);
        if (item == null) {
            return ResponseEntity.status(HttpStatus.NOT_FOUND).body(Map.of("error", "workspace not found"));
        }
        return ResponseEntity.ok(item);
    }

    @PostMapping("/wakeup")
    public ResponseEntity<?> wakeupWorkspace(@RequestBody Map<String, String> body,
                                             @RequestParam(value = "namespace", required = false, defaultValue = "default") String namespace) {
        String userId = body.get("userId");
        if (userId == null || userId.isBlank()) {
            return ResponseEntity.badRequest().body(Map.of("error", "userId is required"));
        }
        WorkspaceItem item = workspaceService.wakeupWorkspace(userId, namespace);
        if (item == null) {
            return ResponseEntity.status(HttpStatus.NOT_FOUND).body(Map.of("error", "workspace not found"));
        }
        return ResponseEntity.ok(item);
    }
}
