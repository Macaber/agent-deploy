package com.example.workspace.apiserver.controller;

import org.springframework.boot.autoconfigure.condition.ConditionalOnProperty;
import org.springframework.stereotype.Controller;
import org.springframework.web.bind.annotation.GetMapping;

@Controller
@ConditionalOnProperty(name = "app.apiserver.enabled", havingValue = "true", matchIfMissing = true)
public class DashboardController {

    @GetMapping("/")
    public String index() {
        return "index";
    }
}
